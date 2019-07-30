package cfapi

import (
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"code.cloudfoundry.org/cli/cf/configuration"
	"code.cloudfoundry.org/cli/cf/configuration/coreconfig"
	"code.cloudfoundry.org/cli/cf/i18n"
	"code.cloudfoundry.org/cli/cf/net"
)

// Session - wraps the CF CLI session objects
type Session struct {
	Log *Logger

	ccInfo CCInfo

	config     coreconfig.Repository
	refresher  coreconfig.APIConfigRefresher
	ccGateway  net.Gateway
	uaaGateway net.Gateway

	authManager      *AuthManager
	stackManager     *StackManager
	userManager      *UserManager
	domainManager    *DomainManager
	asgManager       *ASGManager
	evgManager       *EVGManager
	quotaManager     *QuotaManager
	orgManager       *OrgManager
	spaceManager     *SpaceManager
	serviceManager   *ServiceManager
	buildpackManager *BuildpackManager
	appManager       *AppManager
	routeManager     *RouteManager

	// Used for direct endpoint calls
	httpClient *http.Client
}

// CCInfo -
type CCInfo struct {
	APIEndpoint       string
	User              string
	Password          string
	SkipSslValidation bool

	APIVersion               string `json:"api_version"`
	AuthorizationEndpoint    string `json:"authorization_endpoint"`
	TokenEndpoint            string `json:"token_endpoint"`
	LoggregatorEndpoint      string `json:"logging_endpoint"`
	DopplerEndpoint          string `json:"doppler_logging_endpoint"`
	MinCLIVersion            string `json:"min_cli_version"`
	MinRecommendedCLIVersion string `json:"min_recommended_cli_version"`
	SSHOAuthClient           string `json:"app_ssh_oauth_client"`
	RoutingAPIEndpoint       string `json:"routing_endpoint"`
}

// apiErrResponse -
type apiErrResponse struct {
	Code        int    `json:"code,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
	Description string `json:"description,omitempty"`
}

// uaaErrorResponse -
type uaaErrorResponse struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

// NewSession -
func NewSession(
	endpoint, user, password, uaaClientID, uaaClientSecret, caCert string,
	skipSslValidation bool) (s *Session, err error) {

	s = &Session{
		httpClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: skipSslValidation},
			},
		},
	}

	err = s.initCliConnection(endpoint, user, password, caCert, skipSslValidation)
	if err == nil && len(uaaClientID) > 0 {
		s.userManager.clientToken, err = s.authManager.getClientToken(uaaClientID, uaaClientSecret)
		err = s.userManager.loadGroups()
	}
	return
}

// initCliConnection
func (s *Session) initCliConnection(
	endpoint, user, password, caCert string,
	skipSslValidation bool) (err error) {

	envDialTimeout := os.Getenv("CF_DIAL_TIMEOUT")

	debug, _ := strconv.ParseBool(os.Getenv("CF_DEBUG"))
	s.Log = NewLogger(debug, os.Getenv("CF_TRACE"))

	s.config = coreconfig.NewRepositoryFromPersistor(&noopPersistor{}, func(err error) {
		if err != nil {
			s.Log.UI.Failed(err.Error())
			os.Exit(1)
		}
	})
	if i18n.T == nil {
		i18n.T = i18n.Init(s.config)
	}
	s.config.SetSSLDisabled(skipSslValidation)

	s.ccGateway = net.NewCloudControllerGateway(s.config, time.Now, s.Log.UI, s.Log.TracePrinter, envDialTimeout)
	s.uaaGateway = net.NewUAAGateway(s.config, s.Log.UI, s.Log.TracePrinter, envDialTimeout)
	s.authManager = NewAuthManager(s.uaaGateway, s.config, net.NewRequestDumper(s.Log.TracePrinter))

	endpoint = strings.TrimSuffix(endpoint, "/")
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = "https://" + endpoint
	}

	err = s.ccGateway.GetResource(endpoint+"/v2/info", &s.ccInfo)
	if err != nil {
		return
	}
	s.ccInfo.APIEndpoint = endpoint
	s.ccInfo.User = user
	s.ccInfo.Password = password
	s.ccInfo.SkipSslValidation = skipSslValidation

	s.config.SetAPIEndpoint(endpoint)
	s.config.SetAPIVersion(s.ccInfo.APIVersion)
	s.config.SetAuthenticationEndpoint(s.ccInfo.AuthorizationEndpoint)
	s.config.SetUaaEndpoint(s.ccInfo.TokenEndpoint)
	s.config.SetSSHOAuthClient(s.ccInfo.SSHOAuthClient)
	s.config.SetMinCLIVersion(s.ccInfo.MinCLIVersion)
	s.config.SetMinRecommendedCLIVersion(s.ccInfo.MinRecommendedCLIVersion)
	s.config.SetDopplerEndpoint(s.ccInfo.DopplerEndpoint)
	s.config.SetRoutingAPIEndpoint(s.ccInfo.RoutingAPIEndpoint)

	if len(s.ccInfo.LoggregatorEndpoint) == 0 {
		var endpointDomainRegex = regexp.MustCompile(`^http(s?)://[^\.]+\.([^:]+)`)

		matches := endpointDomainRegex.FindStringSubmatch(endpoint)
		url := fmt.Sprintf("ws%s://loggregator.%s", matches[1], matches[2])
		if url[0:3] == "wss" {
			s.ccInfo.LoggregatorEndpoint = url + ":443"
		} else {
			s.ccInfo.LoggregatorEndpoint = url + ":80"
		}
	}

	err = s.authManager.Authenticate(map[string]string{
		"username": user,
		"password": password,
	})
	if err != nil {
		return err
	}

	s.ccGateway.SetTokenRefresher(s.authManager)
	s.uaaGateway.SetTokenRefresher(s.authManager)

	s.ccGateway.PollingEnabled = false

	s.userManager, err = newUserManager(s.config, s.uaaGateway, s.ccGateway, s.Log)
	if err != nil {
		return err
	}
	s.stackManager, err = newStackManager(s.config, s.ccGateway, s.Log)
	if err != nil {
		return err
	}
	s.domainManager, err = newDomainManager(s.config, s.ccGateway, s.Log)
	if err != nil {
		return err
	}
	s.asgManager, err = newASGManager(s.config, s.ccGateway, s.Log)
	if err != nil {
		return err
	}
	s.evgManager, err = newEVGManager(s.config, s.ccGateway, s.Log)
	if err != nil {
		return err
	}
	s.quotaManager, err = newQuotaManager(s.config, s.ccGateway, s.Log)
	if err != nil {
		return err
	}
	s.orgManager, err = NewOrgManager(s.config, s.ccGateway, s.Log)
	if err != nil {
		return err
	}
	s.spaceManager, err = newSpaceManager(s.config, s.ccGateway, s.Log)
	if err != nil {
		return err
	}
	s.serviceManager, err = newServiceManager(s.config, s.ccGateway, s.Log)
	if err != nil {
		return err
	}
	s.buildpackManager, err = newBuildpackManager(s.config, s.ccGateway, s.Log)
	if err != nil {
		return err
	}
	s.routeManager, err = newRouteManager(s.config, s.ccGateway, s.Log)
	if err != nil {
		return err
	}

	s.appManager, err = newAppManager(s.config, s.ccGateway, s.domainManager.repo, s.routeManager.repo, s.Log)
	if err != nil {
		return err
	}

	return
}

// Info -
func (s *Session) Info() *CCInfo {
	return &s.ccInfo
}

// UserManager -
func (s *Session) UserManager() *UserManager {
	return s.userManager
}

// StackManager -
func (s *Session) StackManager() *StackManager {
	return s.stackManager
}

// DomainManager -
func (s *Session) DomainManager() *DomainManager {
	return s.domainManager
}

// ASGManager -
func (s *Session) ASGManager() *ASGManager {
	return s.asgManager
}

// EVGManager -
func (s *Session) EVGManager() *EVGManager {
	return s.evgManager
}

// QuotaManager -
func (s *Session) QuotaManager() *QuotaManager {
	return s.quotaManager
}

// OrgManager -
func (s *Session) OrgManager() *OrgManager {
	return s.orgManager
}

// SpaceManager -
func (s *Session) SpaceManager() *SpaceManager {
	return s.spaceManager
}

// ServiceManager -
func (s *Session) ServiceManager() *ServiceManager {
	return s.serviceManager
}

// BuildpackManager -
func (s *Session) BuildpackManager() *BuildpackManager {
	return s.buildpackManager
}

// RouteManager -
func (s *Session) RouteManager() *RouteManager {
	return s.routeManager
}

// AppManager -
func (s *Session) AppManager() *AppManager {
	return s.appManager
}

// GetFeatureFlags -
func (s *Session) GetFeatureFlags() (featurFlags map[string]bool, err error) {

	featurFlags = make(map[string]bool)

	response := []interface{}{}
	if err = s.ccGateway.GetResource(
		fmt.Sprintf("%s/v2/config/feature_flags", s.config.APIEndpoint()),
		&response); err != nil {
		return
	}
	for _, v := range response {
		m := v.(map[string]interface{})
		featurFlags[m["name"].(string)] = m["enabled"].(bool)
	}
	return
}

// SetFeatureFlags -
func (s *Session) SetFeatureFlags(featureFlags map[string]bool) (err error) {

	for k, v := range featureFlags {

		if err = s.ccGateway.UpdateResource(s.config.APIEndpoint(),
			fmt.Sprintf("/v2/config/feature_flags/%s", k),
			strings.NewReader(fmt.Sprintf("{\"enabled\":%t}", v))); err != nil {

			return
		}
	}
	return
}

// noopPersistor - No Op Persistor for CF CLI session
type noopPersistor struct {
}

func newNoopPersistor() configuration.Persistor {
	return &noopPersistor{}
}

func (p *noopPersistor) Delete() {
}

func (p *noopPersistor) Exists() bool {
	return false
}

func (p *noopPersistor) Load(configuration.DataInterface) error {
	return nil
}

func (p *noopPersistor) Save(configuration.DataInterface) error {
	return nil
}

// newUUID generates a random UUID according to RFC 4122
func newUUID() (string, error) {
	uuid := make([]byte, 16)
	n, err := io.ReadFull(rand.Reader, uuid)
	if n != len(uuid) || err != nil {
		return "", err
	}

	// variant bits; see section 4.1.1
	uuid[8] = uuid[8]&^0xc0 | 0x80
	// version 4 (pseudo-random); see section 4.1.3
	uuid[6] = uuid[6]&^0xf0 | 0x40
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:]), nil
}

// newRandomString generates a random string of desired length
func newRandomString(n int) (string, error) {
    const letters = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
        bytes := make([]byte, n)
        _, err := rand.Read(bytes)
        if err != nil {
            return "", err
        }
        for i, b := range bytes {
            bytes[i] = letters[b%byte(len(letters))]
        }
        return string(bytes), nil
}