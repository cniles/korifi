package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"code.cloudfoundry.org/cf-k8s-controllers/api/apis"
	"code.cloudfoundry.org/cf-k8s-controllers/api/payloads"
	"code.cloudfoundry.org/cf-k8s-controllers/api/presenter"
	"code.cloudfoundry.org/cf-k8s-controllers/api/tests/e2e/helpers"

	"github.com/go-http-utils/headers"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"
	certsv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	hnsv1alpha2 "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
)

var (
	k8sClient           client.Client
	clientset           *kubernetes.Clientset
	rootNamespace       string
	apiServerRoot       string
	serviceAccountName  string
	serviceAccountToken string
	tokenAuthHeader     string
	certUserName        string
	certSigningReq      *certsv1.CertificateSigningRequest
	certAuthHeader      string
	adminAuthHeader     string
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(helpers.E2EFailHandler)
	RunSpecs(t, "E2E Suite")
}

var _ = BeforeSuite(func() {
	SetDefaultEventuallyTimeout(120 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	apiServerRoot = mustHaveEnv("API_SERVER_ROOT")

	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	Expect(hnsv1alpha2.AddToScheme(scheme.Scheme)).To(Succeed())

	config, err := controllerruntime.GetConfig()
	Expect(err).NotTo(HaveOccurred())

	adminAuthHeader = "ClientCert " + obtainAdminUserCert()

	k8sClient, err = client.New(config, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	clientset, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())

	rootNamespace = mustHaveEnv("ROOT_NAMESPACE")
	ensureServerIsUp()

	serviceAccountName = generateGUID("token-user")
	serviceAccountToken = obtainServiceAccountToken(serviceAccountName)

	certUserName = generateGUID("cert-user")
	var certPEM string
	certSigningReq, certPEM = obtainClientCert(certUserName)
	certAuthHeader = "ClientCert " + certPEM
})

var _ = BeforeEach(func() {
	tokenAuthHeader = fmt.Sprintf("Bearer %s", serviceAccountToken)
})

var _ = AfterSuite(func() {
	deleteServiceAccount(serviceAccountName)
	deleteCSR(certSigningReq)
})

func mustHaveEnv(key string) string {
	val, ok := os.LookupEnv(key)
	Expect(ok).To(BeTrue(), "must set env var %q", key)

	return val
}

func ensureServerIsUp() {
	Eventually(func() (int, error) {
		resp, err := http.Get(apiServerRoot)
		if err != nil {
			return 0, err
		}

		resp.Body.Close()

		return resp.StatusCode, nil
	}, "5m").Should(Equal(http.StatusOK), "API Server at %s was not running after 5 minutes", apiServerRoot)
}

func generateGUID(prefix string) string {
	guid := uuid.NewString()

	return fmt.Sprintf("%s-%s", prefix, guid[:13])
}

func deleteSubnamespace(parent, name string) {
	ctx := context.Background()

	subnsList := &hnsv1alpha2.SubnamespaceAnchorList{}
	Expect(k8sClient.List(ctx, subnsList, client.InNamespace(name))).To(Succeed())

	var wg sync.WaitGroup
	wg.Add(len(subnsList.Items))
	for _, subns := range subnsList.Items {
		asyncDeleteSubnamespace(name, subns.Name, &wg)
	}
	wg.Wait()

	anchor := hnsv1alpha2.SubnamespaceAnchor{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: parent,
			Name:      name,
		},
	}
	err := k8sClient.Delete(ctx, &anchor)
	Expect(err).NotTo(HaveOccurred())

	Eventually(func() bool {
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&anchor), &anchor)

		return errors.IsNotFound(err)
	}).Should(BeTrue())
}

func createOrgRaw(orgName, authHeader string) (*http.Response, error) {
	return httpReq(
		http.MethodPost,
		apiServerRoot+apis.OrgsEndpoint,
		authHeader,
		map[string]interface{}{"name": orgName},
	)
}

func createOrg(orgName, authHeader string) presenter.OrgResponse {
	resp, err := createOrgRaw(orgName, authHeader)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	Expect(resp).To(HaveHTTPStatus(http.StatusCreated))
	Expect(resp).To(HaveHTTPHeaderWithValue(headers.ContentType, "application/json"))

	org := presenter.OrgResponse{}
	err = json.NewDecoder(resp.Body).Decode(&org)
	Expect(err).NotTo(HaveOccurred())

	Eventually(func() ([]rbacv1.RoleBinding, error) {
		roleBindings := rbacv1.RoleBindingList{}
		err := k8sClient.List(context.Background(), &roleBindings, client.InNamespace(org.GUID))
		if err != nil {
			return nil, err
		}
		return roleBindings.Items, nil
	}).Should(ContainElement(HaveRoleBindingTo("cf-k8s-controllers-admin")))

	return org
}

func asyncCreateOrg(orgName, authHeader string, createdOrg *presenter.OrgResponse, wg *sync.WaitGroup, errChan chan error) {
	go func() {
		defer wg.Done()
		defer GinkgoRecover()

		resp, err := createOrgRaw(orgName, authHeader)
		if err != nil {
			errChan <- err
			return
		}

		if resp.StatusCode != http.StatusCreated {
			errChan <- fmt.Errorf("expected status code %d, got %d", http.StatusCreated, resp.StatusCode)
			return
		}

		org := presenter.OrgResponse{}
		err = json.NewDecoder(resp.Body).Decode(&org)
		if err != nil {
			errChan <- err
			return
		}

		*createdOrg = org
	}()
}

func createSpaceRaw(spaceName, orgGUID, authHeader string) (*http.Response, error) {
	spacesURL := apiServerRoot + apis.SpaceCreateEndpoint
	payload := payloads.SpaceCreate{
		Name: spaceName,
		Relationships: payloads.SpaceRelationships{
			Org: payloads.Relationship{
				Data: &payloads.RelationshipData{
					GUID: orgGUID,
				},
			},
		},
	}
	return httpReq(http.MethodPost, spacesURL, authHeader, payload)
}

func createSpace(spaceName, orgGUID, authHeader string) presenter.SpaceResponse {
	resp, err := createSpaceRaw(spaceName, orgGUID, authHeader)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	Expect(resp).To(HaveHTTPStatus(http.StatusCreated))
	Expect(resp).To(HaveHTTPHeaderWithValue(headers.ContentType, "application/json"))

	space := presenter.SpaceResponse{}
	err = json.NewDecoder(resp.Body).Decode(&space)
	Expect(err).NotTo(HaveOccurred())

	Eventually(func() ([]rbacv1.RoleBinding, error) {
		roleBindings := rbacv1.RoleBindingList{}
		err := k8sClient.List(context.Background(), &roleBindings, client.InNamespace(space.GUID))
		if err != nil {
			return nil, err
		}
		return roleBindings.Items, nil
	}).Should(ContainElement(HaveRoleBindingTo("cf-k8s-controllers-admin")))

	return space
}

func asyncCreateSpace(spaceName, orgGUID, authHeader string, createdSpace *presenter.SpaceResponse, wg *sync.WaitGroup, errChan chan error) {
	go func() {
		defer wg.Done()
		defer GinkgoRecover()

		resp, err := createSpaceRaw(spaceName, orgGUID, authHeader)
		if err != nil {
			errChan <- err
			return
		}

		if resp.StatusCode != http.StatusCreated {
			errChan <- fmt.Errorf("expected status code %d, got %d", http.StatusCreated, resp.StatusCode)
			return
		}

		space := presenter.SpaceResponse{}
		err = json.NewDecoder(resp.Body).Decode(&space)
		if err != nil {
			errChan <- err
			return
		}

		*createdSpace = space
	}()
}

func createRoleRaw(roleName, kind, orgSpaceType, userName, orgSpaceGUID, authHeader string) (*http.Response, error) {
	rolesURL := apiServerRoot + apis.RolesEndpoint
	payload := payloads.RoleCreate{
		Type: roleName,
	}

	switch kind {
	case rbacv1.UserKind:
		payload.Relationships.User = &payloads.UserRelationship{
			Data: payloads.UserRelationshipData{
				Username: userName,
			},
		}
	case rbacv1.ServiceAccountKind:
		payload.Relationships.KubernetesServiceAccount = &payloads.Relationship{
			Data: &payloads.RelationshipData{
				GUID: userName,
			},
		}
	default:
		Fail("unexpected Kind " + kind)
	}

	switch orgSpaceType {
	case "organization":
		payload.Relationships.Organization = &payloads.Relationship{
			Data: &payloads.RelationshipData{
				GUID: orgSpaceGUID,
			},
		}
	case "space":
		payload.Relationships.Space = &payloads.Relationship{
			Data: &payloads.RelationshipData{
				GUID: orgSpaceGUID,
			},
		}
	default:
		Fail("unexpected type " + orgSpaceType)
	}

	return httpReq(http.MethodPost, rolesURL, authHeader, payload)
}

// createRole creates an org or space role
// You should probably invoke this via createOrgRole or createSpaceRole
func createRole(roleName, kind, orgSpaceType, userName, orgSpaceGUID, authHeader string) presenter.RoleResponse {
	resp, err := createRoleRaw(roleName, kind, orgSpaceType, userName, orgSpaceGUID, authHeader)
	ExpectWithOffset(3, err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	ExpectWithOffset(3, resp).To(HaveHTTPStatus(http.StatusCreated))

	role := presenter.RoleResponse{}
	err = json.NewDecoder(resp.Body).Decode(&role)
	ExpectWithOffset(3, err).NotTo(HaveOccurred())

	return role
}

func createOrgRole(roleName, kind, userName, orgGUID, authHeader string) presenter.RoleResponse {
	return createRole(roleName, kind, "organization", userName, orgGUID, authHeader)
}

func createSpaceRole(roleName, kind, userName, spaceGUID, authHeader string) presenter.RoleResponse {
	return createRole(roleName, kind, "space", userName, spaceGUID, authHeader)
}

func obtainServiceAccountToken(name string) string {
	var err error

	serviceAccount := corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: rootNamespace,
		},
	}
	err = k8sClient.Create(context.Background(), &serviceAccount)
	Expect(err).NotTo(HaveOccurred())

	Eventually(func() error {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(&serviceAccount), &serviceAccount); err != nil {
			return err
		}

		if len(serviceAccount.Secrets) != 1 {
			return fmt.Errorf("expected exactly 1 secret, got %d", len(serviceAccount.Secrets))
		}

		return nil
	}, "120s").Should(Succeed())

	tokenSecret := corev1.Secret{}
	Eventually(func() error {
		return k8sClient.Get(context.Background(), client.ObjectKey{Name: serviceAccount.Secrets[0].Name, Namespace: rootNamespace}, &tokenSecret)
	}).Should(Succeed())

	return string(tokenSecret.Data["token"])
}

func deleteServiceAccount(name string) {
	serviceAccount := corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: rootNamespace,
		},
	}

	Expect(k8sClient.Delete(context.Background(), &serviceAccount)).To(Succeed())
}

func obtainClientCert(name string) (*certsv1.CertificateSigningRequest, string) {
	privKey, err := rsa.GenerateKey(rand.Reader, 1024)
	Expect(err).NotTo(HaveOccurred())

	template := x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: name},
		SignatureAlgorithm: x509.SHA256WithRSA,
	}

	csrBytes, err := x509.CreateCertificateRequest(rand.Reader, &template, privKey)
	Expect(err).NotTo(HaveOccurred())

	k8sCSR := &certsv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: uuid.NewString(),
		},
		Spec: certsv1.CertificateSigningRequestSpec{
			Request:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrBytes}),
			SignerName: "kubernetes.io/kube-apiserver-client",
			Usages:     []certsv1.KeyUsage{certsv1.UsageClientAuth},
		},
	}

	Expect(k8sClient.Create(context.Background(), k8sCSR)).To(Succeed())

	k8sCSR.Status.Conditions = append(k8sCSR.Status.Conditions, certsv1.CertificateSigningRequestCondition{
		Type:   certsv1.CertificateApproved,
		Status: "True",
	})

	k8sCSR, err = clientset.CertificatesV1().CertificateSigningRequests().UpdateApproval(context.Background(), k8sCSR.Name, k8sCSR, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())

	var certPEM []byte
	Eventually(func() ([]byte, error) {
		err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(k8sCSR), k8sCSR)
		if err != nil {
			return nil, err
		}

		if len(k8sCSR.Status.Certificate) == 0 {
			return nil, nil
		}

		certPEM = k8sCSR.Status.Certificate

		return certPEM, nil
	}).ShouldNot(BeEmpty())

	buf := bytes.NewBuffer(certPEM)
	Expect(pem.Encode(buf, &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privKey),
	})).To(Succeed())

	return k8sCSR, base64.StdEncoding.EncodeToString(buf.Bytes())
}

func obtainAdminUserCert() string {
	crtBytes, err := base64.StdEncoding.DecodeString(mustHaveEnv("CF_ADMIN_CERT"))
	Expect(err).NotTo(HaveOccurred())
	keyBytes, err := base64.StdEncoding.DecodeString(mustHaveEnv("CF_ADMIN_KEY"))
	Expect(err).NotTo(HaveOccurred())

	return base64.StdEncoding.EncodeToString(append(crtBytes, keyBytes...))
}

func deleteCSR(csr *certsv1.CertificateSigningRequest) {
	Expect(k8sClient.Delete(context.Background(), csr)).To(Succeed())
}

func httpReq(method, url, authHeader string, jsonBody interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	if jsonBody != nil {
		body, err := json.Marshal(jsonBody)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Authorization", authHeader)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func createAppRaw(spaceGUID, name, authHeader string) (*http.Response, error) {
	appsURL := apiServerRoot + apis.AppCreateEndpoint

	payload := payloads.AppCreate{
		Name: name,
		Relationships: payloads.AppRelationships{
			Space: payloads.Relationship{
				Data: &payloads.RelationshipData{
					GUID: spaceGUID,
				},
			},
		},
	}

	return httpReq(http.MethodPost, appsURL, authHeader, payload)
}

func createApp(spaceGUID, name, authHeader string) presenter.AppResponse {
	resp, err := createAppRaw(spaceGUID, name, authHeader)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	Expect(resp).To(HaveHTTPStatus(http.StatusCreated))

	app := presenter.AppResponse{}
	err = json.NewDecoder(resp.Body).Decode(&app)
	Expect(err).NotTo(HaveOccurred())

	return app
}

func createPackage(appGUID, authHeader string) presenter.PackageResponse {
	packagesURL := apiServerRoot + apis.PackageCreateEndpoint

	payload := payloads.PackageCreate{
		Type: "bits",
		Relationships: &payloads.PackageRelationships{
			App: &payloads.Relationship{
				Data: &payloads.RelationshipData{
					GUID: appGUID,
				},
			},
		},
	}

	resp, err := httpReq(http.MethodPost, packagesURL, authHeader, payload)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	Expect(resp).To(HaveHTTPStatus(http.StatusCreated))

	pkg := presenter.PackageResponse{}
	err = json.NewDecoder(resp.Body).Decode(&pkg)
	Expect(err).NotTo(HaveOccurred())

	return pkg
}

func createBuild(packageGUID, authHeader string) presenter.BuildResponse {
	buildsURL := apiServerRoot + apis.BuildCreateEndpoint

	payload := payloads.BuildCreate{
		Package: &payloads.RelationshipData{
			GUID: packageGUID,
		},
	}

	resp, err := httpReq(http.MethodPost, buildsURL, authHeader, payload)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	Expect(resp).To(HaveHTTPStatus(http.StatusCreated))

	build := presenter.BuildResponse{}
	err = json.NewDecoder(resp.Body).Decode(&build)
	Expect(err).NotTo(HaveOccurred())

	return build
}

func setCurrentDroplet(appGUID, dropletGUID, authHeader string) presenter.CurrentDropletResponse {
	setDropletURL := apiServerRoot + "/v3/apps/" + appGUID + "/relationships/current_droplet"

	payload := payloads.AppSetCurrentDroplet{
		Relationship: payloads.Relationship{
			Data: &payloads.RelationshipData{
				GUID: dropletGUID,
			},
		},
	}

	resp, err := httpReq(http.MethodPatch, setDropletURL, authHeader, payload)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	Expect(resp).To(HaveHTTPStatus(http.StatusOK))

	currentDropletResponse := presenter.CurrentDropletResponse{}
	err = json.NewDecoder(resp.Body).Decode(&currentDropletResponse)
	Expect(err).NotTo(HaveOccurred())

	return currentDropletResponse
}

func asyncDeleteSubnamespace(orgID, spaceID string, wg *sync.WaitGroup) {
	go func() {
		defer wg.Done()
		defer GinkgoRecover()

		deleteSubnamespace(orgID, spaceID)
	}()
}

func get(endpoint string, authHeaderValue string) (map[string]interface{}, error) {
	return getWithQuery(endpoint, authHeaderValue, nil)
}

func getWithQuery(endpoint string, authHeaderValue string, query map[string]string) (map[string]interface{}, error) {
	serverUrl, err := url.Parse(apiServerRoot)
	if err != nil {
		return nil, err
	}

	serverUrl.Path = endpoint
	values := url.Values{}
	for key, val := range query {
		values.Set(key, val)
	}
	serverUrl.RawQuery = values.Encode()

	resp, err := httpReq(http.MethodGet, serverUrl.String(), authHeaderValue, nil)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status: %d", resp.StatusCode)
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	response := map[string]interface{}{}
	err = json.Unmarshal(bodyBytes, &response)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func patch(endpoint string, authHeaderValue string, payload interface{}) (map[string]interface{}, error) {
	serverUrl, err := url.Parse(apiServerRoot)
	if err != nil {
		return nil, err
	}

	serverUrl.Path = endpoint

	resp, err := httpReq(http.MethodPatch, serverUrl.String(), authHeaderValue, payload)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status: %d", resp.StatusCode)
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	response := map[string]interface{}{}
	err = json.Unmarshal(bodyBytes, &response)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func uploadNodeApp(pkgGUID, authHeader string) {
	var b bytes.Buffer
	writer := multipart.NewWriter(&b)
	part, err := writer.CreateFormFile("bits", "node.zip")
	Expect(err).NotTo(HaveOccurred())

	file, err := os.Open("assets/node.zip")
	Expect(err).NotTo(HaveOccurred())
	defer file.Close()

	_, err = io.Copy(part, file)
	Expect(err).NotTo(HaveOccurred())
	Expect(writer.Close()).To(Succeed())

	uploadURL := apiServerRoot + fmt.Sprintf("/v3/packages/%s/upload", pkgGUID)
	req, err := http.NewRequestWithContext(context.Background(), "POST", uploadURL, &b)
	Expect(err).NotTo(HaveOccurred())
	req.Header.Add("Content-Type", writer.FormDataContentType())
	req.Header.Add("Authorization", authHeader)

	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp).To(HaveHTTPStatus(http.StatusOK))
}

type roleBindingToRoleMatcher struct {
	roleName string
}

func HaveRoleBindingTo(roleName string) gomegatypes.GomegaMatcher {
	return &roleBindingToRoleMatcher{roleName: roleName}
}

func (m *roleBindingToRoleMatcher) Match(actual interface{}) (bool, error) {
	roleBinding, ok := actual.(rbacv1.RoleBinding)
	if !ok {
		return false, fmt.Errorf("roleBindingToRoleMatcher matcher expects an rbacv1.RoleBinding")
	}

	return roleBinding.RoleRef.Name == m.roleName, nil
}

func (m *roleBindingToRoleMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected\n\t%#v\nto be bond to role\n\t%#v", actual, m.roleName)
}

func (m *roleBindingToRoleMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected\n\t%#v\nnot to be bond to role\n\t%#v", actual, m.roleName)
}
