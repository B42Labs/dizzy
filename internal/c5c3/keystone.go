package c5c3

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	gcconfig "github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"gopkg.in/yaml.v2"

	"github.com/B42Labs/dizzy/internal/resource"
)

// maxPasswordRejections is how many 401 answers to password logins one Client
// takes before it sends no further password login. Keystone can lock an
// account after repeated failed logins, 10 within 300 s on the deployment the
// c5c3 namespace is built for. A run sends its user two password logins that
// must fail and further failed ones only where a check has already failed, so
// five leave room for both while a lockout stays out of reach.
const maxPasswordRejections = 5

// errPasswordLoginLimit is what a password login wraps when the client
// refuses to send it because Keystone has already answered
// maxPasswordRejections password logins with 401. It is not one of the
// package's sentinels, so a refused login reads as error kind other.
var errPasswordLoginLimit = errors.New("c5c3: password login not sent")

// publicInterface is the catalog interface VerifyCatalog looks the ordered
// endpoint up under.
const publicInterface = "public"

// SetKeystoneEndpoint makes every Keystone check log in at authURL instead of
// the auth_url of the delivered document, and trust Keystone through tlsConfig
// instead of the TLS settings the document implies. An empty authURL keeps the
// delivered auth_url and a nil tlsConfig the document's TLS settings, so a
// client that never calls it sends each document as delivered. Call it before
// the first check; it is not safe to call while one runs.
func (c *Client) SetKeystoneEndpoint(authURL string, tlsConfig *tls.Config) {
	c.keystoneURL = authURL
	c.keystoneTLS = tlsConfig
}

// Authenticate logs in once with the document cred delivers, as it was
// delivered: unscoped for a user, whose document names no project, and scoped
// by Keystone to the credential's project for an application credential. With
// a non-empty wantProject the token's project name must equal it, otherwise
// the result wraps ErrMismatch. A 401, and a 404 to an application credential
// login, is classified against the order the credential belongs to (see
// classify). It records one authenticate sample of kind over the login.
func (c *Client) Authenticate(ctx context.Context, kind resource.Kind, cred Credential, wantProject string) error {
	l, err := c.prepareLogin(cred, nil)
	if err != nil {
		c.record(ctx, kind, "authenticate", 0, err)
		return err
	}
	before := c.readDelivery(ctx, cred)
	start := time.Now()
	result, err := c.sendLogin(ctx, l)
	d := time.Since(start)
	switch {
	case err != nil:
		err = c.classify(ctx, cred, before, err)
	case wantProject != "":
		err = checkProject(result, cred, wantProject)
	}
	c.record(ctx, kind, "authenticate", d, err)
	return err
}

// VerifyRole logs in once with the document cred delivers, scoped to the
// project projectName in the domain domainName, and checks that the token
// carries the role named role. A token without it wraps ErrMismatch, and a
// 401, or a 404 to an application credential login, is classified against the
// order the credential belongs to (see classify). It records one validate
// sample of kind keystone_role_assignment over the login. An empty project or
// domain name is refused without a request, as a failed validate sample: the
// scope would fall back to the domain.
func (c *Client) VerifyRole(ctx context.Context, cred Credential, projectName, domainName, role string) error {
	scope, err := projectScope(projectName, domainName)
	if err != nil {
		c.record(ctx, KindRoleAssignment, "validate", 0, err)
		return err
	}
	l, err := c.prepareLogin(cred, scope)
	if err != nil {
		c.record(ctx, KindRoleAssignment, "validate", 0, err)
		return err
	}
	before := c.readDelivery(ctx, cred)
	start := time.Now()
	result, err := c.sendLogin(ctx, l)
	d := time.Since(start)
	if err != nil {
		err = c.classify(ctx, cred, before, err)
	} else {
		err = checkRole(result, cred, projectName, role)
	}
	c.record(ctx, KindRoleAssignment, "validate", d, err)
	return err
}

// VerifyCatalog logs in with the document cred delivers, scoped to the
// project projectName in the domain domainName, again and again at the
// client's poll intervals, until a token's catalog lists a service of type
// serviceType named serviceName with a public endpoint whose URL is
// endpointURL. Keystone's catalog cache can lag the order turning ready, which
// is why it polls. A retryable failure is polled through; a 401, or a 404 to
// an application credential login, ends the poll classified against the order
// the credential belongs to (see classify), and any other failure ends it as
// it is. It records one validate sample of kind keystone_catalog_entry over
// the whole poll.
//
// At the context's deadline it returns ErrMismatch, wrapping the deadline,
// only when the last login completed with a token whose catalog lacked the
// entry. Otherwise it returns the error of the latest login that failed with a
// retryable error after the last token, and the deadline itself when there is
// none, as when the deadline cut short the only login after a token without
// the entry: an outage is never a mismatch, and an earlier token without the
// entry does not make one. So that the deadline cuts short only a login that
// hangs, not one that was merely under way, the poll sends no login in the
// last tenth of the time the context left it at the start, nor while less than
// twice its longest completed login is left. An empty project or domain name
// is refused without a request, as a failed validate sample.
func (c *Client) VerifyCatalog(ctx context.Context, cred Credential, projectName, domainName, serviceType, serviceName, endpointURL string) error {
	scope, err := projectScope(projectName, domainName)
	if err != nil {
		c.record(ctx, KindCatalogEntry, "validate", 0, err)
		return err
	}
	l, err := c.prepareLogin(cred, scope)
	if err != nil {
		c.record(ctx, KindCatalogEntry, "validate", 0, err)
		return err
	}

	start := time.Now()
	deadline, hasDeadline := ctx.Deadline()
	var (
		// lacking is true when the last login completed with a token whose
		// catalog lacked the entry.
		lacking bool
		// lastErr is the retryable error of the latest login that failed with
		// one, nil when a later login issued a token.
		lastErr error
		// reserve is the time before the deadline in which no login is sent,
		// and longest the longest a completed login has taken.
		reserve, longest time.Duration
	)
	if hasDeadline {
		reserve = time.Until(deadline) / 10
	}
	err = c.poll(ctx, func(ctx context.Context) (bool, error) {
		if hasDeadline && time.Until(deadline) < max(reserve, 2*longest) {
			// Too close to the deadline for a login to complete: the last
			// login sent decides, and poll returns ctx.Err() at the deadline.
			return false, nil
		}
		begin := time.Now()
		before := c.readDelivery(ctx, cred)
		result, err := c.sendLogin(ctx, l)
		if ctx.Err() == nil {
			longest = max(longest, time.Since(begin))
		}
		switch {
		case err == nil:
		case ctx.Err() != nil:
			// The context's end cut the login short, which says nothing about
			// the catalog: no earlier token decides the result, and poll
			// returns ctx.Err().
			lacking = false
			return false, nil
		case IsRetryable(err):
			lacking, lastErr = false, err
			return false, nil
		default:
			return false, c.classify(ctx, cred, before, err)
		}
		lacking, lastErr = false, nil
		catalog, err := result.ExtractServiceCatalog()
		if err != nil {
			return false, fmt.Errorf("reading the catalog of the token issued with %v: %w", cred, err)
		}
		if listsEndpoint(catalog, serviceType, serviceName, endpointURL) {
			return true, nil
		}
		lacking = true
		return false, nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		switch {
		case lastErr != nil:
			err = fmt.Errorf("the last login before the deadline failed: %w", lastErr)
		case lacking:
			err = fmt.Errorf("%w: no token scoped to project %q listed a %s service %q with the public endpoint %s: %w",
				ErrMismatch, projectName, serviceType, serviceName, endpointURL, err)
		}
	}
	c.record(ctx, KindCatalogEntry, "validate", time.Since(start), err)
	return err
}

// Reject sends one login with the document cred delivers, which Keystone must
// refuse: a superseded password or a deleted application credential. A 401 is
// success, and so is a 404 for an application credential, which is how
// Keystone answers a login with a credential id it does not know. A token
// wraps ErrAccepted. Any other failure, such as a 5xx or the context's end, is
// returned as it is, and Reject never sends a second login. It records one
// reject sample of kind over the login.
func (c *Client) Reject(ctx context.Context, kind resource.Kind, cred Credential) error {
	l, err := c.prepareLogin(cred, nil)
	if err != nil {
		c.record(ctx, kind, "reject", 0, err)
		return err
	}
	start := time.Now()
	_, err = c.sendLogin(ctx, l)
	d := time.Since(start)
	switch status := httpStatus(err); {
	case err == nil:
		err = fmt.Errorf("%w: Keystone issued a token for %v of order %s, which it must reject", ErrAccepted, cred, cred.Order.ID)
	case status == http.StatusUnauthorized,
		status == http.StatusNotFound && kind == KindApplicationCredential:
		err = nil
	}
	c.record(ctx, kind, "reject", d, err)
	return err
}

// loginRequest is one login prepared from a delivered document: the
// credential the document came from, the options parsed from it, and the TLS
// configuration to send them with.
type loginRequest struct {
	cred Credential
	opts gophercloud.AuthOptions
	tls  *tls.Config
}

// password reports whether the login authenticates with a password, a login
// Keystone counts toward an account lockout when it fails.
func (l loginRequest) password() bool {
	return l.opts.Password != ""
}

// secretValues returns every secret the login carries, from the Secret's own
// keys and from the document, which could disagree.
func (l loginRequest) secretValues() []string {
	return []string{l.cred.Password, l.cred.ApplicationCredentialSecret, l.opts.Password, l.opts.ApplicationCredentialSecret}
}

// prepareLogin parses the document cred delivers into the options of one
// login. clouds.Parse reads the document as the only clouds.yaml and its one
// entry as the cloud, so no clouds.yaml or secure.yaml file from disk or named
// by the environment takes part, and OS_CLOUD and OS_CLIENT_CONFIG_FILE have
// no effect; a clouds-public.yaml would take part only for an entry naming a
// profile, which no delivered document does. Other environment variables still
// apply to each login. clouds.Parse computes the TLS configuration from
// OS_CACERT, OS_CERT and OS_KEY as well as from the document: an exported
// OS_CACERT changes whom a login trusts unless SetKeystoneEndpoint gave a TLS
// configuration, and a file one of them names that cannot be read, or an
// OS_CERT without an OS_KEY, fails every login, with an override or without.
// It also reads OS_REGION_NAME and OS_INTERFACE, into endpoint options no
// login uses.
//
// The endpoint override of SetKeystoneEndpoint replaces the document's
// auth_url and the TLS configuration, a non-nil scope replaces the scope the
// document implies, and re-authentication is off, so one login is one token
// request.
//
// A document gophercloud cannot decode wraps ErrMismatch without the
// decoder's text, which can quote a value of the document; any other parse
// failure, such as an unreadable CA file, is returned wrapped.
func (c *Client) prepareLogin(cred Credential, scope *gophercloud.AuthScope) (loginRequest, error) {
	var doc clouds.Clouds
	if err := yaml.Unmarshal(cred.CloudsYAML, &doc); err != nil {
		return loginRequest{}, fmt.Errorf("%w: the clouds.yaml delivered beside order %s is not a document gophercloud can read",
			ErrMismatch, cred.Order.ID)
	}
	parseOpts := []clouds.ParseOption{
		clouds.WithCloudsYAML(bytes.NewReader(cred.CloudsYAML)),
		clouds.WithCloudName(cred.Cloud),
	}
	if c.keystoneURL != "" {
		parseOpts = append(parseOpts, clouds.WithIdentityEndpoint(c.keystoneURL))
	}
	if scope != nil {
		parseOpts = append(parseOpts, clouds.WithScope(scope))
	}
	opts, _, tlsConfig, err := clouds.Parse(parseOpts...)
	if err != nil {
		return loginRequest{}, fmt.Errorf("parsing the clouds.yaml delivered beside order %s: %w", cred.Order.ID, err)
	}
	if c.keystoneTLS != nil {
		tlsConfig = c.keystoneTLS
	}
	opts.AllowReauth = false
	return loginRequest{cred: cred, opts: opts, tls: tlsConfig}, nil
}

// sendLogin sends l as one token request through gcconfig.NewProviderClient
// and returns the result the token's project, roles and catalog are read
// from. Once Keystone has answered maxPasswordRejections password logins of
// this client with 401, a password login is refused without a request, and
// every 401 to a password login counts toward that limit. The counter is safe
// for concurrent logins; logins already in flight when it reaches the limit
// are still answered.
//
// A failed login is returned wrapped. gophercloud's error quotes the URL and
// Keystone's answer, never the request; the text is still cut of every secret
// l carries, in case an answer echoes one.
func (c *Client) sendLogin(ctx context.Context, l loginRequest) (tokens.CreateResult, error) {
	if l.password() {
		if n := c.passwordRejections.Load(); n >= maxPasswordRejections {
			return tokens.CreateResult{}, fmt.Errorf("%w: Keystone answered %d password logins of this run with 401, "+
				"and more failed logins can make Keystone lock the account of order %s", errPasswordLoginLimit, n, l.cred.Order.ID)
		}
	}
	provider, err := gcconfig.NewProviderClient(ctx, l.opts, gcconfig.WithTLSConfig(l.tls))
	if err != nil {
		if l.password() && httpStatus(err) == http.StatusUnauthorized {
			c.passwordRejections.Add(1)
		}
		return tokens.CreateResult{}, fmt.Errorf("issuing a token with %v: %w", l.cred, redact(err, l.secretValues()...))
	}
	if l.tls != nil {
		// WithTLSConfig gave this login a transport of its own. Close its
		// connection now rather than leave it idle until the transport's
		// timeout.
		provider.HTTPClient.CloseIdleConnections()
	}
	result, ok := provider.GetAuthResult().(tokens.CreateResult)
	if !ok {
		return tokens.CreateResult{}, fmt.Errorf("issuing a token with %v: the login returned a %T, not a token", l.cred, provider.GetAuthResult())
	}
	return result, nil
}

// redactedError is a login's error with every secret of the login cut from
// its text. It unwraps to the original error, so the status and the error
// kind stay readable.
type redactedError struct {
	err  error
	text string
}

// Error returns the text with the secrets cut out.
func (e *redactedError) Error() string { return e.text }

// Unwrap returns the original error.
func (e *redactedError) Unwrap() error { return e.err }

// redact returns err with each non-empty secret replaced by [redacted] in its
// text, and err itself when the text carries none of them.
func redact(err error, secrets ...string) error {
	original := err.Error()
	text := original
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "[redacted]")
		}
	}
	if text == original {
		return err
	}
	return &redactedError{err: err, text: text}
}

// delivery is what the classification of a rejected login reads of the order
// a credential belongs to: the order's status and the resourceVersion of the
// Secret beside it, or the error of the read that failed.
type delivery struct {
	status          OrderStatus
	resourceVersion string
	err             error
}

// readDelivery reads the status of cred's order, one get sample of its kind,
// and the Secret beside it, one get sample of kind secret. A failed read is
// kept in the result rather than returned: it only stops a rejected login
// from being called a mismatch.
func (c *Client) readDelivery(ctx context.Context, cred Credential) delivery {
	st, err := c.Status(ctx, cred.Order)
	if err != nil {
		return delivery{err: err}
	}
	rv, err := c.secretVersion(ctx, cred.Order)
	if err != nil {
		return delivery{err: err}
	}
	return delivery{status: st, resourceVersion: rv}
}

// secretVersion reads the Secret beside order with the GET ReadCredential
// sends, as one get sample of kind secret, and returns its resourceVersion. A
// 404 is a failed get here: the Secret of a ready order must exist.
func (c *Client) secretVersion(ctx context.Context, order resource.Resource) (string, error) {
	s, err := resolveSecret(order)
	if err != nil {
		return "", err
	}
	u, d, err := c.getSecret(ctx, s)
	c.record(ctx, KindSecret, "get", d, err)
	if err != nil {
		return "", fmt.Errorf("reading secret %s: %w", s.id(), err)
	}
	return u.GetResourceVersion(), nil
}

// classify decides what err, the failure of a login with cred, means, given
// before, what readDelivery read just before the login. It classifies the
// answers Keystone rejects a credential with: a 401 to any login, and a 404 to
// an application credential login, which is how Keystone answers an
// application credential id it does not know. Anything else is returned as it
// is: a 5xx, a timeout or a connection error is an outage, which the caller
// may retry, never a mismatch, and a 404 to a password login is no rejection
// of the password.
//
// A rejection reads the order and its Secret again. It wraps ErrMismatch when
// the credential was valid by the order's own account throughout the login:
// for a user, the order was ready before and after and the Secret stayed at
// the credential's resourceVersion; for an application credential, the
// order's credentialID or previousCredentialID named the credential's id
// before and after. Otherwise the login raced a change, such as a rotation,
// and the answer is returned as it is, with error kind http_401 or http_404.
// So is a rejection around which a read failed, since it cannot be
// classified.
func (c *Client) classify(ctx context.Context, cred Credential, before delivery, err error) error {
	status := httpStatus(err)
	rejected := status == http.StatusUnauthorized ||
		status == http.StatusNotFound && cred.Order.Kind == KindApplicationCredential
	if !rejected {
		return err
	}
	after := c.readDelivery(ctx, cred)
	readErr := before.err
	if readErr == nil {
		readErr = after.err
	}
	if readErr != nil {
		return fmt.Errorf("%w; order %s could not be read around the login, so the %d is not taken for a mismatch: %v",
			err, cred.Order.ID, status, readErr)
	}
	switch cred.Order.Kind {
	case KindUser:
		if before.status.Ready && after.status.Ready &&
			before.resourceVersion == cred.ResourceVersion && after.resourceVersion == cred.ResourceVersion {
			return fmt.Errorf("%w: Keystone rejected the password delivered beside order %s, "+
				"which was ready before and after the login with its secret unchanged at resourceVersion %s: %w",
				ErrMismatch, cred.Order.ID, cred.ResourceVersion, err)
		}
	case KindApplicationCredential:
		id := cred.ApplicationCredentialID
		if namesCredential(before.status, id) && namesCredential(after.status, id) {
			return fmt.Errorf("%w: Keystone rejected application credential %s with a %d, "+
				"and order %s named it before and after the login: %w",
				ErrMismatch, id, status, cred.Order.ID, err)
		}
	}
	return fmt.Errorf("%w; order %s or its secret changed around the login, so the %d is not taken for a mismatch",
		err, cred.Order.ID, status)
}

// namesCredential reports whether st names the application credential id as
// the delivered or the superseded one.
func namesCredential(st OrderStatus, id string) bool {
	return id != "" && (st.CredentialID == id || st.PreviousCredentialID == id)
}

// projectScope returns the scope of a login to the project projectName in the
// domain domainName. Both must be set: without a project name gophercloud
// would scope the token to the domain.
func projectScope(projectName, domainName string) (*gophercloud.AuthScope, error) {
	if projectName == "" || domainName == "" {
		return nil, fmt.Errorf("a project-scoped login needs a project name and a domain name, got %q and %q", projectName, domainName)
	}
	return &gophercloud.AuthScope{ProjectName: projectName, DomainName: domainName}, nil
}

// checkProject returns ErrMismatch unless the token of result, issued with
// cred, is scoped to the project named want.
func checkProject(result tokens.CreateResult, cred Credential, want string) error {
	project, err := result.ExtractProject()
	if err != nil {
		return fmt.Errorf("reading the project of the token issued with %v: %w", cred, err)
	}
	if project == nil {
		return fmt.Errorf("%w: the token issued with %v of order %s is scoped to no project, want project %q",
			ErrMismatch, cred, cred.Order.ID, want)
	}
	if project.Name != want {
		return fmt.Errorf("%w: the token issued with %v of order %s is scoped to project %q, want project %q",
			ErrMismatch, cred, cred.Order.ID, project.Name, want)
	}
	return nil
}

// checkRole returns ErrMismatch unless the token of result, issued with cred
// and scoped to the project named project, carries the role named role.
func checkRole(result tokens.CreateResult, cred Credential, project, role string) error {
	roles, err := result.ExtractRoles()
	if err != nil {
		return fmt.Errorf("reading the roles of the token issued with %v: %w", cred, err)
	}
	if slices.ContainsFunc(roles, func(r tokens.Role) bool { return r.Name == role }) {
		return nil
	}
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, r.Name)
	}
	return fmt.Errorf("%w: the token issued with %v of order %s for project %q carries the roles %q, not %q",
		ErrMismatch, cred, cred.Order.ID, project, names, role)
}

// listsEndpoint reports whether catalog lists a service of type serviceType
// named serviceName with a public endpoint whose URL is endpointURL.
func listsEndpoint(catalog *tokens.ServiceCatalog, serviceType, serviceName, endpointURL string) bool {
	for _, entry := range catalog.Entries {
		if entry.Type != serviceType || entry.Name != serviceName {
			continue
		}
		for _, ep := range entry.Endpoints {
			if ep.Interface == publicInterface && ep.URL == endpointURL {
				return true
			}
		}
	}
	return false
}
