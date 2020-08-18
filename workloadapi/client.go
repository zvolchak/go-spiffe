package workloadapi

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/logger"
	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Client is a Workload API client.
type Client struct {
	conn     *grpc.ClientConn
	wlClient workload.SpiffeWorkloadAPIClient
	config   clientConfig
	backoff  *backoff
}

// New dials the Workload API and returns a client.
func New(ctx context.Context, options ...ClientOption) (*Client, error) {
	c := &Client{
		config:  defaultClientConfig(),
		backoff: newBackoff(),
	}
	for _, opt := range options {
		opt.configureClient(&c.config)
	}

	err := c.setAddress()
	if err != nil {
		return nil, err
	}

	c.conn, err = c.newConn(ctx)
	if err != nil {
		return nil, err
	}

	c.wlClient = workload.NewSpiffeWorkloadAPIClient(c.conn)
	return c, nil
}

// Close closes the client.
func (c *Client) Close() error {
	return c.conn.Close()
}

// FetchX509SVID fetches the default X509-SVID, i.e. the first in the list
// returned by the Workload API.
func (c *Client) FetchX509SVID(ctx context.Context) (*x509svid.SVID, error) {
	ctx, cancel := context.WithCancel(withHeader(ctx))
	defer cancel()

	stream, err := c.wlClient.FetchX509SVID(ctx, &workload.X509SVIDRequest{})
	if err != nil {
		return nil, err
	}

	resp, err := stream.Recv()
	if err != nil {
		return nil, err
	}

	svids, err := parseX509SVIDs(resp, true)
	if err != nil {
		return nil, err
	}

	return svids[0], nil
}

// FetchX509SVIDs fetches all X509-SVIDs.
func (c *Client) FetchX509SVIDs(ctx context.Context) ([]*x509svid.SVID, error) {
	ctx, cancel := context.WithCancel(withHeader(ctx))
	defer cancel()

	stream, err := c.wlClient.FetchX509SVID(ctx, &workload.X509SVIDRequest{})
	if err != nil {
		return nil, err
	}

	resp, err := stream.Recv()
	if err != nil {
		return nil, err
	}

	return parseX509SVIDs(resp, false)
}

// FetchX509Bundles fetches the X.509 bundles.
func (c *Client) FetchX509Bundles(ctx context.Context) (*x509bundle.Set, error) {
	ctx, cancel := context.WithCancel(withHeader(ctx))
	defer cancel()

	stream, err := c.wlClient.FetchX509SVID(ctx, &workload.X509SVIDRequest{})
	if err != nil {
		return nil, err
	}
	resp, err := stream.Recv()
	if err != nil {
		return nil, err
	}

	return parseX509Bundles(resp)
}

// FetchX509Context fetches the X.509 context, which contains both X509-SVIDs
// and X.509 bundles.
func (c *Client) FetchX509Context(ctx context.Context) (*X509Context, error) {
	ctx, cancel := context.WithCancel(withHeader(ctx))
	defer cancel()

	stream, err := c.wlClient.FetchX509SVID(ctx, &workload.X509SVIDRequest{})
	if err != nil {
		return nil, err
	}

	resp, err := stream.Recv()
	if err != nil {
		return nil, err
	}

	return parseX509Context(resp)
}

// WatchX509Context watches for updates to the X.509 context. The watcher
// receives the updated X.509 context.
func (c *Client) WatchX509Context(ctx context.Context, watcher X509ContextWatcher) error {
	for {
		err := c.watchX509Context(ctx, watcher)
		watcher.OnX509ContextWatchError(err)
		err = c.handleWatchError(ctx, err)
		if err != nil {
			return err
		}
	}
}

// FetchJWTSVID fetches a JWT-SVID.
func (c *Client) FetchJWTSVID(ctx context.Context, params jwtsvid.Params) (*jwtsvid.SVID, error) {
	ctx, cancel := context.WithCancel(withHeader(ctx))
	defer cancel()

	audience := append([]string{params.Audience}, params.ExtraAudiences...)
	resp, err := c.wlClient.FetchJWTSVID(ctx, &workload.JWTSVIDRequest{
		SpiffeId: params.Subject.String(),
		Audience: audience,
	})
	if err != nil {
		return nil, err
	}

	if len(resp.Svids) == 0 {
		return nil, errors.New("there were no SVIDs in the response")
	}
	return jwtsvid.ParseInsecure(resp.Svids[0].Svid, audience)
}

// FetchJWTBundles fetches the JWT bundles for JWT-SVID validation, keyed
// by a SPIFFE ID of the trust domain to which they belong.
func (c *Client) FetchJWTBundles(ctx context.Context) (*jwtbundle.Set, error) {
	ctx, cancel := context.WithCancel(withHeader(ctx))
	defer cancel()

	stream, err := c.wlClient.FetchJWTBundles(ctx, &workload.JWTBundlesRequest{})
	if err != nil {
		return nil, err
	}

	resp, err := stream.Recv()
	if err != nil {
		return nil, err
	}

	return parseJWTSVIDBundles(resp)
}

// WatchJWTBundles watches for changes to the JWT bundles. The watcher receives
// the updated JWT bundles.
func (c *Client) WatchJWTBundles(ctx context.Context, watcher JWTBundleWatcher) error {
	for {
		err := c.watchJWTBundles(ctx, watcher)
		watcher.OnJWTBundlesWatchError(err)
		err = c.handleWatchError(ctx, err)
		if err != nil {
			return err
		}
	}
}

// ValidateJWTSVID validates the JWT-SVID token. The parsed and validated
// JWT-SVID is returned.
func (c *Client) ValidateJWTSVID(ctx context.Context, token, audience string) (*jwtsvid.SVID, error) {
	ctx, cancel := context.WithCancel(withHeader(ctx))
	defer cancel()

	_, err := c.wlClient.ValidateJWTSVID(ctx, &workload.ValidateJWTSVIDRequest{
		Svid:     token,
		Audience: audience,
	})
	if err != nil {
		return nil, err
	}

	return jwtsvid.ParseInsecure(token, []string{audience})
}

func (c *Client) setAddress() error {
	if c.config.address == "" {
		var ok bool
		c.config.address, ok = GetDefaultAddress()
		if !ok {
			return errors.New("workload endpoint socket address is not configured")
		}
	}

	var err error
	c.config.address, err = parseTargetFromAddr(c.config.address)
	return err
}

func (c *Client) newConn(ctx context.Context) (*grpc.ClientConn, error) {
	c.config.dialOptions = append(c.config.dialOptions, grpc.WithInsecure())
	return grpc.DialContext(ctx, c.config.address, c.config.dialOptions...)
}

func (c *Client) handleWatchError(ctx context.Context, err error) error {
	code := status.Code(err)
	if code == codes.Canceled {
		return err
	}

	if code == codes.InvalidArgument {
		c.config.log.Errorf("Canceling watch: %v", err)
		return err
	}

	c.config.log.Errorf("Failed to watch the Workload API: %v", err)
	backoff := c.backoff.Duration()
	c.config.log.Debugf("Retrying watch in %s", backoff)
	select {
	case <-time.After(backoff):
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) watchX509Context(ctx context.Context, watcher X509ContextWatcher) error {
	ctx, cancel := context.WithCancel(withHeader(ctx))
	defer cancel()

	c.config.log.Debugf("Watching X.509 contexts")
	stream, err := c.wlClient.FetchX509SVID(ctx, &workload.X509SVIDRequest{})
	if err != nil {
		return err
	}

	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}

		c.backoff.Reset()
		x509Context, err := parseX509Context(resp)
		if err != nil {
			c.config.log.Errorf("Failed to parse X509-SVID response: %v", err)
			watcher.OnX509ContextWatchError(err)
			continue
		}
		watcher.OnX509ContextUpdate(x509Context)
	}
}

func (c *Client) watchJWTBundles(ctx context.Context, watcher JWTBundleWatcher) error {
	ctx, cancel := context.WithCancel(withHeader(ctx))
	defer cancel()

	c.config.log.Debugf("Watching JWT bundles")
	stream, err := c.wlClient.FetchJWTBundles(ctx, &workload.JWTBundlesRequest{})
	if err != nil {
		return err
	}

	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}

		c.backoff.Reset()
		jwtbundleSet, err := parseJWTSVIDBundles(resp)
		if err != nil {
			c.config.log.Errorf("Failed to parse JWT bundle response: %v", err)
			watcher.OnJWTBundlesWatchError(err)
			continue
		}
		watcher.OnJWTBundlesUpdate(jwtbundleSet)
	}
}

// X509ContextWatcher receives X509Context updates from the Workload API.
type X509ContextWatcher interface {
	// OnX509ContextUpdate is called with the latest X.509 context retrieved
	// from the Workload API.
	OnX509ContextUpdate(*X509Context)

	// OnX509ContextWatchError is called when there is a problem establishing
	// or maintaining connectivity with the Workload API.
	OnX509ContextWatchError(error)
}

// JWTBundleWatcher receives JWT bundle updates from the Workload API.
type JWTBundleWatcher interface {
	// OnJWTBundlesUpdate is called with the latest JWT bundle set retrieved
	// from the Workload API.
	OnJWTBundlesUpdate(*jwtbundle.Set)

	// OnJWTBundlesWatchError is called when there is a problem establishing
	// or maintaining connectivity with the Workload API.
	OnJWTBundlesWatchError(error)
}

func withHeader(ctx context.Context) context.Context {
	header := metadata.Pairs("workload.spiffe.io", "true")
	return metadata.NewOutgoingContext(ctx, header)
}

func defaultClientConfig() clientConfig {
	return clientConfig{
		log: logger.Null,
	}
}

func parseX509Context(resp *workload.X509SVIDResponse) (*X509Context, error) {
	svids, err := parseX509SVIDs(resp, false)
	if err != nil {
		return nil, err
	}

	bundles, err := parseX509Bundles(resp)
	if err != nil {
		return nil, err
	}

	return &X509Context{
		SVIDs:   svids,
		Bundles: bundles,
	}, nil
}

// parseX509SVIDs parses one or all of the SVIDs in the response. If firstOnly
// is true, then only the first SVID in the response is parsed and returned.
// Otherwise all SVIDs are parsed and returned.
func parseX509SVIDs(resp *workload.X509SVIDResponse, firstOnly bool) ([]*x509svid.SVID, error) {
	n := len(resp.Svids)
	if firstOnly {
		n = 1
	}

	svids := make([]*x509svid.SVID, 0, n)
	for i := 0; i < n; i++ {
		svid := resp.Svids[i]
		s, err := x509svid.ParseRaw(svid.X509Svid, svid.X509SvidKey)
		if err != nil {
			return nil, err
		}
		svids = append(svids, s)
	}

	if len(svids) == 0 {
		return nil, errors.New("no SVIDs in response")
	}
	return svids, nil
}

func parseX509Bundles(resp *workload.X509SVIDResponse) (*x509bundle.Set, error) {
	bundles := []*x509bundle.Bundle{}
	for _, svid := range resp.Svids {
		b, err := parseX509Bundle(svid.SpiffeId, svid.Bundle)
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, b)
	}

	for tdID, bundle := range resp.FederatedBundles {
		b, err := parseX509Bundle(tdID, bundle)
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, b)
	}

	return x509bundle.NewSet(bundles...), nil
}

func parseX509Bundle(spiffeID string, bundle []byte) (*x509bundle.Bundle, error) {
	td, err := spiffeid.TrustDomainFromString(spiffeID)
	if err != nil {
		return nil, err
	}
	certs, err := x509.ParseCertificates(bundle)
	if err != nil {
		return nil, err
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("empty X.509 bundle for trust domain %q", td)
	}
	return x509bundle.FromX509Authorities(td, certs), nil
}

func parseJWTSVIDBundles(resp *workload.JWTBundlesResponse) (*jwtbundle.Set, error) {
	bundles := []*jwtbundle.Bundle{}

	for tdID, b := range resp.Bundles {
		td, err := spiffeid.TrustDomainFromString(tdID)
		if err != nil {
			return nil, err
		}

		b, err := jwtbundle.Parse(td, b)
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, b)
	}

	return jwtbundle.NewSet(bundles...), nil
}