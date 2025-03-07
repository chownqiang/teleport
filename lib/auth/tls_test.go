/*
Copyright 2017-2020 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"context"
	"crypto"
	"crypto/tls"
	"encoding/base32"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/jonboulle/clockwork"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/breaker"
	"github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/constants"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	apiutils "github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/api/utils/sshutils"
	"github.com/gravitational/teleport/lib/auth/native"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/fixtures"
	"github.com/gravitational/teleport/lib/jwt"
	"github.com/gravitational/teleport/lib/modules"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/services/suite"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"
)

type authContext struct {
	dataDir string
	server  *TestTLSServer
	clock   clockwork.FakeClock
}

func setupAuthContext(ctx context.Context, t *testing.T) *authContext {
	var tt authContext
	t.Cleanup(func() { tt.Close() })

	tt.dataDir = t.TempDir()
	tt.clock = clockwork.NewFakeClock()

	testAuthServer, err := NewTestAuthServer(TestAuthServerConfig{
		Dir:   tt.dataDir,
		Clock: tt.clock,
	})
	require.NoError(t, err)

	tt.server, err = testAuthServer.NewTestTLSServer()
	require.NoError(t, err)

	return &tt
}

func (a *authContext) Close() error {
	return a.server.Close()
}

// TestRemoteBuiltinRole tests remote builtin role
// that gets mapped to remote proxy readonly role
func TestRemoteBuiltinRole(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	remoteServer, err := NewTestAuthServer(TestAuthServerConfig{
		Dir:         t.TempDir(),
		ClusterName: "remote",
		Clock:       tt.clock,
	})
	require.NoError(t, err)

	certPool, err := tt.server.CertPool()
	require.NoError(t, err)

	// without trust, proxy server will get rejected
	// remote auth server will get rejected because it is not supported
	remoteProxy, err := remoteServer.NewRemoteClient(
		TestBuiltin(types.RoleProxy), tt.server.Addr(), certPool)
	require.NoError(t, err)

	// certificate authority is not recognized, because
	// the trust has not been established yet
	_, err = remoteProxy.GetNodes(ctx, apidefaults.Namespace)
	require.True(t, trace.IsConnectionProblem(err))

	// after trust is established, things are good
	err = tt.server.AuthServer.Trust(ctx, remoteServer, nil)
	require.NoError(t, err)

	// re initialize client with trust established.
	remoteProxy, err = remoteServer.NewRemoteClient(
		TestBuiltin(types.RoleProxy), tt.server.Addr(), certPool)
	require.NoError(t, err)

	_, err = remoteProxy.GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// remote auth server will get rejected even with established trust
	remoteAuth, err := remoteServer.NewRemoteClient(
		TestBuiltin(types.RoleAuth), tt.server.Addr(), certPool)
	require.NoError(t, err)

	_, err = remoteAuth.GetDomainName(ctx)
	require.True(t, trace.IsAccessDenied(err))
}

// TestAcceptedUsage tests scenario when server is set up
// to accept certificates with certain usage metadata restrictions
// encoded
func TestAcceptedUsage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	server, err := NewTestAuthServer(TestAuthServerConfig{
		Dir:           t.TempDir(),
		ClusterName:   "remote",
		AcceptedUsage: []string{"usage:k8s"},
		Clock:         tt.clock,
	})
	require.NoError(t, err)

	user, _, err := CreateUserAndRole(server.AuthServer, "user", []string{"role"})
	require.NoError(t, err)

	tlsServer, err := server.NewTestTLSServer()
	require.NoError(t, err)
	defer tlsServer.Close()

	// Unrestricted clients can use restricted servers
	client, err := tlsServer.NewClient(TestUser(user.GetName()))
	require.NoError(t, err)

	// certificate authority is not recognized, because
	// the trust has not been established yet
	_, err = client.GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// restricted clients can use restricted servers if restrictions
	// exactly match
	identity := TestUser(user.GetName())
	identity.AcceptedUsage = []string{"usage:k8s"}
	client, err = tlsServer.NewClient(identity)
	require.NoError(t, err)

	_, err = client.GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// restricted clients can will be rejected if usage does not match
	identity = TestUser(user.GetName())
	identity.AcceptedUsage = []string{"usage:extra"}
	client, err = tlsServer.NewClient(identity)
	require.NoError(t, err)

	_, err = client.GetNodes(ctx, apidefaults.Namespace)
	require.True(t, trace.IsAccessDenied(err))

	// restricted clients can will be rejected, for now if there is any mismatch,
	// including extra usage.
	identity = TestUser(user.GetName())
	identity.AcceptedUsage = []string{"usage:k8s", "usage:unknown"}
	client, err = tlsServer.NewClient(identity)
	require.NoError(t, err)

	_, err = client.GetNodes(ctx, apidefaults.Namespace)
	require.True(t, trace.IsAccessDenied(err))
}

// TestRemoteRotation tests remote builtin role
// that attempts certificate authority rotation
func TestRemoteRotation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	var ok bool

	remoteServer, err := NewTestAuthServer(TestAuthServerConfig{
		Dir:         t.TempDir(),
		ClusterName: "remote",
		Clock:       tt.clock,
	})
	require.NoError(t, err)

	certPool, err := tt.server.CertPool()
	require.NoError(t, err)

	// after trust is established, things are good
	err = tt.server.AuthServer.Trust(ctx, remoteServer, nil)
	require.NoError(t, err)

	remoteProxy, err := remoteServer.NewRemoteClient(
		TestBuiltin(types.RoleProxy), tt.server.Addr(), certPool)
	require.NoError(t, err)

	remoteAuth, err := remoteServer.NewRemoteClient(
		TestBuiltin(types.RoleAuth), tt.server.Addr(), certPool)
	require.NoError(t, err)

	// remote cluster starts rotation
	gracePeriod := time.Hour
	remoteServer.AuthServer.privateKey, ok = fixtures.PEMBytes["rsa"]
	require.Equal(t, ok, true)
	err = remoteServer.AuthServer.RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseInit,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// moves to update clients
	err = remoteServer.AuthServer.RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseUpdateClients,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	remoteCA, err := remoteServer.AuthServer.GetCertAuthority(ctx, types.CertAuthID{
		DomainName: remoteServer.ClusterName,
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)

	// remote proxy should be rejected when trying to rotate ca
	// that is not associated with the remote cluster
	clone := remoteCA.Clone()
	clone.SetName(tt.server.ClusterName())
	err = remoteProxy.RotateExternalCertAuthority(ctx, clone)
	require.True(t, trace.IsAccessDenied(err))

	// remote proxy can't upsert the certificate authority,
	// only to rotate it (in remote rotation only certain fields are updated)
	err = remoteProxy.UpsertCertAuthority(remoteCA)
	require.True(t, trace.IsAccessDenied(err))

	// remote proxy can't read local cert authority with secrets
	_, err = remoteProxy.GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, true)
	require.True(t, trace.IsAccessDenied(err))

	// no secrets read is allowed
	_, err = remoteProxy.GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)

	// remote auth server will get rejected
	err = remoteAuth.RotateExternalCertAuthority(ctx, remoteCA)
	require.True(t, trace.IsAccessDenied(err))

	// remote proxy should be able to perform remote cert authority
	// rotation
	err = remoteProxy.RotateExternalCertAuthority(ctx, remoteCA)
	require.NoError(t, err)

	// newRemoteProxy should be trusted by the auth server
	newRemoteProxy, err := remoteServer.NewRemoteClient(
		TestBuiltin(types.RoleProxy), tt.server.Addr(), certPool)
	require.NoError(t, err)

	_, err = newRemoteProxy.GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// old proxy client is still trusted
	_, err = tt.server.CloneClient(remoteProxy).GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)
}

// TestLocalProxyPermissions tests new local proxy permissions
// as it's now allowed to update host cert authorities of remote clusters
func TestLocalProxyPermissions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	remoteServer, err := NewTestAuthServer(TestAuthServerConfig{
		Dir:         t.TempDir(),
		ClusterName: "remote",
		Clock:       tt.clock,
	})
	require.NoError(t, err)

	// after trust is established, things are good
	err = tt.server.AuthServer.Trust(ctx, remoteServer, nil)
	require.NoError(t, err)

	ca, err := tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)

	proxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	// local proxy can't update local cert authorities
	err = proxy.UpsertCertAuthority(ca)
	require.True(t, trace.IsAccessDenied(err))

	// local proxy is allowed to update host CA of remote cert authorities
	remoteCA, err := tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: remoteServer.ClusterName,
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)

	err = proxy.UpsertCertAuthority(remoteCA)
	require.NoError(t, err)
}

// TestAutoRotation tests local automatic rotation
func TestAutoRotation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	var ok bool

	// create proxy client
	proxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	// client works before rotation is initiated
	_, err = proxy.GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// starts rotation
	tt.server.Auth().privateKey, ok = fixtures.PEMBytes["rsa"]
	require.Equal(t, ok, true)
	gracePeriod := time.Hour
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		Mode:        types.RotationModeAuto,
	})
	require.NoError(t, err)

	// advance rotation by clock
	tt.clock.Advance(gracePeriod/3 + time.Minute)
	err = tt.server.Auth().autoRotateCertAuthorities(ctx)
	require.NoError(t, err)

	ca, err := tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)
	require.Equal(t, ca.GetRotation().Phase, types.RotationPhaseUpdateClients)

	// old clients should work
	_, err = tt.server.CloneClient(proxy).GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// new clients work as well
	_, err = tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	// advance rotation by clock
	tt.clock.Advance((gracePeriod*2)/3 + time.Minute)
	err = tt.server.Auth().autoRotateCertAuthorities(ctx)
	require.NoError(t, err)

	ca, err = tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)
	require.Equal(t, ca.GetRotation().Phase, types.RotationPhaseUpdateServers)

	// old clients should work
	_, err = tt.server.CloneClient(proxy).GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// new clients work as well
	newProxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	_, err = newProxy.GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// complete rotation - advance rotation by clock
	tt.clock.Advance(gracePeriod/3 + time.Minute)
	err = tt.server.Auth().autoRotateCertAuthorities(ctx)
	require.NoError(t, err)
	ca, err = tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)
	require.Equal(t, ca.GetRotation().Phase, types.RotationPhaseStandby)
	require.NoError(t, err)

	// old clients should no longer work
	// new client has to be created here to force re-create the new
	// connection instead of re-using the one from pool
	// this is not going to be a problem in real teleport
	// as it reloads the full server after reload
	_, err = tt.server.CloneClient(proxy).GetNodes(ctx, apidefaults.Namespace)
	require.ErrorContains(t, err, "bad certificate")

	// new clients work
	_, err = tt.server.CloneClient(newProxy).GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)
}

// TestAutoFallback tests local automatic rotation fallback,
// when user intervenes with rollback and rotation gets switched
// to manual mode
func TestAutoFallback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	var ok bool

	// create proxy client just for test purposes
	proxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	// client works before rotation is initiated
	_, err = proxy.GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// starts rotation
	tt.server.Auth().privateKey, ok = fixtures.PEMBytes["rsa"]
	require.Equal(t, ok, true)
	gracePeriod := time.Hour
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		Mode:        types.RotationModeAuto,
	})
	require.NoError(t, err)

	// advance rotation by clock
	tt.clock.Advance(gracePeriod/3 + time.Minute)
	err = tt.server.Auth().autoRotateCertAuthorities(ctx)
	require.NoError(t, err)

	ca, err := tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)
	require.Equal(t, ca.GetRotation().Phase, types.RotationPhaseUpdateClients)
	require.Equal(t, ca.GetRotation().Mode, types.RotationModeAuto)

	// rollback rotation
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseRollback,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	ca, err = tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)
	require.Equal(t, ca.GetRotation().Phase, types.RotationPhaseRollback)
	require.Equal(t, ca.GetRotation().Mode, types.RotationModeManual)
}

// TestManualRotation tests local manual rotation
// that performs full-cycle certificate authority rotation
func TestManualRotation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	var ok bool

	// create proxy client just for test purposes
	proxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	// client works before rotation is initiated
	_, err = proxy.GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// can't jump to mid-phase
	gracePeriod := time.Hour
	tt.server.Auth().privateKey, ok = fixtures.PEMBytes["rsa"]
	require.Equal(t, ok, true)
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseUpdateServers,
		Mode:        types.RotationModeManual,
	})
	require.True(t, trace.IsBadParameter(err))

	// starts rotation
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseInit,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// old clients should work
	_, err = tt.server.CloneClient(proxy).GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// clients reconnect
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseUpdateClients,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// old clients should work
	_, err = tt.server.CloneClient(proxy).GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// new clients work as well
	newProxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	_, err = newProxy.GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// can't jump to standy
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseStandby,
		Mode:        types.RotationModeManual,
	})
	require.True(t, trace.IsBadParameter(err))

	// advance rotation:
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseUpdateServers,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// old clients should work
	_, err = tt.server.CloneClient(proxy).GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// new clients work as well
	_, err = tt.server.CloneClient(newProxy).GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// complete rotation
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseStandby,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// old clients should no longer work
	// new client has to be created here to force re-create the new
	// connection instead of re-using the one from pool
	// this is not going to be a problem in real teleport
	// as it reloads the full server after reload
	_, err = tt.server.CloneClient(proxy).GetNodes(ctx, apidefaults.Namespace)
	require.ErrorContains(t, err, "bad certificate")

	// new clients work
	_, err = tt.server.CloneClient(newProxy).GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)
}

// TestRollback tests local manual rotation rollback
func TestRollback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	var ok bool

	// create proxy client just for test purposes
	proxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	// client works before rotation is initiated
	_, err = proxy.GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// starts rotation
	gracePeriod := time.Hour
	tt.server.Auth().privateKey, ok = fixtures.PEMBytes["rsa"]
	require.Equal(t, ok, true)
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseInit,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// move to update clients phase
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseUpdateClients,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// new clients work
	newProxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	_, err = newProxy.GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// advance rotation:
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseUpdateServers,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// rollback rotation
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseRollback,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// new clients work, server still accepts the creds
	// because new clients should re-register and receive new certs
	_, err = tt.server.CloneClient(newProxy).GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)

	// can't jump to other phases
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseUpdateClients,
		Mode:        types.RotationModeManual,
	})
	require.True(t, trace.IsBadParameter(err))

	// complete rollback
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseStandby,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// clients with new creds will no longer work
	_, err = tt.server.CloneClient(newProxy).GetNodes(ctx, apidefaults.Namespace)
	require.ErrorContains(t, err, "bad certificate")

	// clients with old creds will still work
	_, err = tt.server.CloneClient(proxy).GetNodes(ctx, apidefaults.Namespace)
	require.NoError(t, err)
}

// TestAppTokenRotation checks that JWT tokens can be rotated and tokens can or
// can not be validated at the appropriate phase.
func TestAppTokenRotation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	client, err := tt.server.NewClient(TestBuiltin(types.RoleApp))
	require.NoError(t, err)

	// Create a JWT using the current CA, this will become the "old" CA during
	// rotation.
	oldJWT, err := client.GenerateAppToken(context.Background(),
		types.GenerateAppTokenRequest{
			Username: "foo",
			Roles:    []string{"bar", "baz"},
			URI:      "http://localhost:8080",
			Expires:  tt.clock.Now().Add(1 * time.Minute),
		})
	require.NoError(t, err)

	// Check that the "old" CA can be used to verify tokens.
	oldCA, err := tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.JWTSigner,
	}, true)
	require.NoError(t, err)
	require.Len(t, oldCA.GetTrustedJWTKeyPairs(), 1)

	// Verify that the JWT token validates with the JWT authority.
	_, err = verifyJWT(tt.clock, tt.server.ClusterName(), oldCA.GetTrustedJWTKeyPairs(), oldJWT)
	require.NoError(t, err)

	// Start rotation and move to initial phase. A new CA will be added (for
	// verification), but requests will continue to be signed by the old CA.
	gracePeriod := time.Hour
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.JWTSigner,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseInit,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// At this point in rotation, two JWT key pairs should exist.
	oldCA, err = tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.JWTSigner,
	}, true)
	require.NoError(t, err)
	require.Equal(t, oldCA.GetRotation().Phase, types.RotationPhaseInit)
	require.Len(t, oldCA.GetTrustedJWTKeyPairs(), 2)

	// Verify that the JWT token validates with the JWT authority.
	_, err = verifyJWT(tt.clock, tt.server.ClusterName(), oldCA.GetTrustedJWTKeyPairs(), oldJWT)
	require.NoError(t, err)

	// Move rotation into the update client phase. In this phase, requests will
	// be signed by the new CA, but the old CA will be around to verify requests.
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.JWTSigner,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseUpdateClients,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// New tokens should now fail to validate with the old key.
	newJWT, err := client.GenerateAppToken(ctx,
		types.GenerateAppTokenRequest{
			Username: "foo",
			Roles:    []string{"bar", "baz"},
			URI:      "http://localhost:8080",
			Expires:  tt.clock.Now().Add(1 * time.Minute),
		})
	require.NoError(t, err)

	// New tokens will validate with the new key.
	newCA, err := tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.JWTSigner,
	}, true)
	require.NoError(t, err)
	require.Equal(t, newCA.GetRotation().Phase, types.RotationPhaseUpdateClients)
	require.Len(t, newCA.GetTrustedJWTKeyPairs(), 2)

	// Both JWT should now validate.
	_, err = verifyJWT(tt.clock, tt.server.ClusterName(), newCA.GetTrustedJWTKeyPairs(), oldJWT)
	require.NoError(t, err)
	_, err = verifyJWT(tt.clock, tt.server.ClusterName(), newCA.GetTrustedJWTKeyPairs(), newJWT)
	require.NoError(t, err)

	// Move rotation into update servers phase.
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.JWTSigner,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseUpdateServers,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// At this point only the phase on the CA should have changed.
	newCA, err = tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.JWTSigner,
	}, true)
	require.NoError(t, err)
	require.Equal(t, newCA.GetRotation().Phase, types.RotationPhaseUpdateServers)
	require.Len(t, newCA.GetTrustedJWTKeyPairs(), 2)

	// Both JWT should continue to validate.
	_, err = verifyJWT(tt.clock, tt.server.ClusterName(), newCA.GetTrustedJWTKeyPairs(), oldJWT)
	require.NoError(t, err)
	_, err = verifyJWT(tt.clock, tt.server.ClusterName(), newCA.GetTrustedJWTKeyPairs(), newJWT)
	require.NoError(t, err)

	// Complete rotation. The old CA will be removed.
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.JWTSigner,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseStandby,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	// The new CA should now only have a single key.
	newCA, err = tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.JWTSigner,
	}, true)
	require.NoError(t, err)
	require.Equal(t, newCA.GetRotation().Phase, types.RotationPhaseStandby)
	require.Len(t, newCA.GetTrustedJWTKeyPairs(), 1)

	// Old token should no longer validate.
	_, err = verifyJWT(tt.clock, tt.server.ClusterName(), newCA.GetTrustedJWTKeyPairs(), oldJWT)
	require.Error(t, err)
	_, err = verifyJWT(tt.clock, tt.server.ClusterName(), newCA.GetTrustedJWTKeyPairs(), newJWT)
	require.NoError(t, err)
}

// TestRemoteUser tests scenario when remote user connects to the local
// auth server and some edge cases.
func TestRemoteUser(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	remoteServer, err := NewTestAuthServer(TestAuthServerConfig{
		Dir:         t.TempDir(),
		ClusterName: "remote",
		Clock:       tt.clock,
	})
	require.NoError(t, err)

	remoteUser, remoteRole, err := CreateUserAndRole(remoteServer.AuthServer, "remote-user", []string{"remote-role"})
	require.NoError(t, err)

	certPool, err := tt.server.CertPool()
	require.NoError(t, err)

	remoteClient, err := remoteServer.NewRemoteClient(
		TestUser(remoteUser.GetName()), tt.server.Addr(), certPool)
	require.NoError(t, err)

	// User is not authorized to perform any actions
	// as local cluster does not trust the remote cluster yet
	_, err = remoteClient.GetDomainName(ctx)
	require.True(t, trace.IsConnectionProblem(err))

	// Establish trust, the request will still fail, there is
	// no role mapping set up
	err = tt.server.AuthServer.Trust(ctx, remoteServer, nil)
	require.NoError(t, err)

	// Create fresh client now trust is established
	remoteClient, err = remoteServer.NewRemoteClient(
		TestUser(remoteUser.GetName()), tt.server.Addr(), certPool)
	require.NoError(t, err)
	_, err = remoteClient.GetDomainName(ctx)
	require.True(t, trace.IsAccessDenied(err))

	// Establish trust and map remote role to local admin role
	_, localRole, err := CreateUserAndRole(tt.server.Auth(), "local-user", []string{"local-role"})
	require.NoError(t, err)

	err = tt.server.AuthServer.Trust(ctx, remoteServer, types.RoleMap{{Remote: remoteRole.GetName(), Local: []string{localRole.GetName()}}})
	require.NoError(t, err)

	_, err = remoteClient.GetDomainName(ctx)
	require.NoError(t, err)
}

// TestNopUser tests user with no permissions except
// the ones that require other authentication methods ("nop" user)
func TestNopUser(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	client, err := tt.server.NewClient(TestNop())
	require.NoError(t, err)

	// Nop User can get cluster name
	_, err = client.GetDomainName(ctx)
	require.NoError(t, err)

	// But can not get users or nodes
	_, err = client.GetUsers(false)
	require.True(t, trace.IsAccessDenied(err))

	_, err = client.GetNodes(ctx, apidefaults.Namespace)
	require.True(t, trace.IsAccessDenied(err))

	// Endpoints that allow current user access should return access denied to
	// the Nop user.
	err = client.CheckPassword("foo", nil, "")
	require.True(t, trace.IsAccessDenied(err))
}

// TestOwnRole tests that user can read roles assigned to them (used by web UI)
func TestReadOwnRole(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	user1, userRole, err := CreateUserAndRoleWithoutRoles(clt, "user1", []string{"user1"})
	require.NoError(t, err)

	user2, _, err := CreateUserAndRoleWithoutRoles(clt, "user2", []string{"user2"})
	require.NoError(t, err)

	// user should be able to read their own roles
	userClient, err := tt.server.NewClient(TestUser(user1.GetName()))
	require.NoError(t, err)

	_, err = userClient.GetRole(ctx, userRole.GetName())
	require.NoError(t, err)

	// user2 can't read user1 role
	userClient2, err := tt.server.NewClient(TestIdentity{I: LocalUser{Username: user2.GetName()}})
	require.NoError(t, err)

	_, err = userClient2.GetRole(ctx, userRole.GetName())
	require.True(t, trace.IsAccessDenied(err))
}

func TestGetCurrentUser(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	srv := newTestTLSServer(t)

	user1, _, err := CreateUserAndRole(srv.Auth(), "user1", []string{"user1"})
	require.NoError(t, err)

	client1, err := srv.NewClient(TestIdentity{I: LocalUser{Username: user1.GetName()}})
	require.NoError(t, err)

	currentUser, err := client1.GetCurrentUser(ctx)
	require.NoError(t, err)
	require.Equal(t, &types.UserV2{
		Kind:    "user",
		SubKind: "",
		Version: "v2",
		Metadata: types.Metadata{
			Name:        "user1",
			Namespace:   "default",
			Description: "",
			Labels:      nil,
			Expires:     nil,
			ID:          currentUser.GetMetadata().ID,
		},
		Spec: types.UserSpecV2{
			Roles: []string{"user:user1"},
		},
	}, currentUser)
}

func TestGetCurrentUserRoles(t *testing.T) {
	ctx := context.Background()
	srv := newTestTLSServer(t)

	user1, user1Role, err := CreateUserAndRole(srv.Auth(), "user1", []string{"user-role"})
	require.NoError(t, err)

	client1, err := srv.NewClient(TestIdentity{I: LocalUser{Username: user1.GetName()}})
	require.NoError(t, err)

	roles, err := client1.GetCurrentUserRoles(ctx)
	require.NoError(t, err)
	require.Empty(t, cmp.Diff(roles, []types.Role{user1Role}, cmpopts.IgnoreFields(types.Metadata{}, "ID")))
}

func TestAuthPreferenceSettings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	suite := &suite.ServicesTestSuite{
		ConfigS: clt,
	}
	suite.AuthPreference(t)
}

func TestTunnelConnectionsCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	suite := &suite.ServicesTestSuite{
		PresenceS: clt,
		Clock:     clockwork.NewFakeClock(),
	}
	suite.TunnelConnectionsCRUD(t)
}

func TestRemoteClustersCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	suite := &suite.ServicesTestSuite{
		PresenceS: clt,
	}
	suite.RemoteClustersCRUD(t)
}

func TestServersCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	suite := &suite.ServicesTestSuite{
		PresenceS: clt,
	}
	suite.ServerCRUD(t)
}

// TestAppServerCRUD tests CRUD functionality for services.App using an auth client.
func TestAppServerCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestBuiltin(types.RoleApp))
	require.NoError(t, err)

	suite := &suite.ServicesTestSuite{
		PresenceS: clt,
	}
	suite.AppServerCRUD(t)
}

func TestReverseTunnelsCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	suite := &suite.ServicesTestSuite{
		PresenceS: clt,
	}
	suite.ReverseTunnelsCRUD(t)
}

func TestUsersCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	err = clt.UpsertPassword("user1", []byte("some pass"))
	require.NoError(t, err)

	users, err := clt.GetUsers(false)
	require.NoError(t, err)
	require.Equal(t, len(users), 1)
	require.Equal(t, users[0].GetName(), "user1")

	require.NoError(t, clt.DeleteUser(context.TODO(), "user1"))

	users, err = clt.GetUsers(false)
	require.NoError(t, err)
	require.Equal(t, len(users), 0)
}

func TestPasswordGarbage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)
	garbage := [][]byte{
		nil,
		make([]byte, defaults.MaxPasswordLength+1),
		make([]byte, defaults.MinPasswordLength-1),
	}
	for _, g := range garbage {
		err := clt.CheckPassword("user1", g, "123456")
		require.True(t, trace.IsBadParameter(err))
	}
}

func TestPasswordCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	pass := []byte("abc123")
	rawSecret := "def456"
	otpSecret := base32.StdEncoding.EncodeToString([]byte(rawSecret))

	err = clt.CheckPassword("user1", pass, "123456")
	require.Error(t, err)

	err = clt.UpsertPassword("user1", pass)
	require.NoError(t, err)

	dev, err := services.NewTOTPDevice("otp", otpSecret, tt.clock.Now())
	require.NoError(t, err)

	err = tt.server.Auth().UpsertMFADevice(ctx, "user1", dev)
	require.NoError(t, err)

	validToken, err := totp.GenerateCode(otpSecret, tt.server.Clock().Now())
	require.NoError(t, err)

	err = clt.CheckPassword("user1", pass, validToken)
	require.NoError(t, err)
}

func TestTokens(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	out, err := clt.GenerateToken(ctx, &proto.GenerateTokenRequest{Roles: types.SystemRoles{types.RoleNode}})
	require.NoError(t, err)
	require.NotEqual(t, out, 0)
}

func TestOTPCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	user := "user1"
	pass := []byte("abc123")
	rawSecret := "def456"
	otpSecret := base32.StdEncoding.EncodeToString([]byte(rawSecret))

	// upsert a password and totp secret
	err = clt.UpsertPassword("user1", pass)
	require.NoError(t, err)
	dev, err := services.NewTOTPDevice("otp", otpSecret, tt.clock.Now())
	require.NoError(t, err)

	err = tt.server.Auth().UpsertMFADevice(ctx, user, dev)
	require.NoError(t, err)

	// a completely invalid token should return access denied
	err = clt.CheckPassword("user1", pass, "123456")
	require.Error(t, err)

	// an invalid token should return access denied
	//
	// this tests makes the token 61 seconds in the future (but from a valid key)
	// even though the validity period is 30 seconds. this is because a token is
	// valid for 30 seconds + 30 second skew before and after for a usability
	// reasons. so a token made between seconds 31 and 60 is still valid, and
	// invalidity starts at 61 seconds in the future.
	invalidToken, err := totp.GenerateCode(otpSecret, tt.server.Clock().Now().Add(61*time.Second))
	require.NoError(t, err)
	err = clt.CheckPassword("user1", pass, invalidToken)
	require.Error(t, err)

	// a valid token (created right now and from a valid key) should return success
	validToken, err := totp.GenerateCode(otpSecret, tt.server.Clock().Now())
	require.NoError(t, err)

	err = clt.CheckPassword("user1", pass, validToken)
	require.NoError(t, err)

	// try the same valid token now it should fail because we don't allow re-use of tokens
	err = clt.CheckPassword("user1", pass, validToken)
	require.Error(t, err)
}

// TestWebSessions tests web sessions flow for web user,
// that logs in, extends web session and tries to perform administratvie action
// but fails
func TestWebSessionWithoutAccessRequest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	user := "user1"
	pass := []byte("abc123")

	_, _, err = CreateUserAndRole(clt, user, []string{user})
	require.NoError(t, err)

	proxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	req := AuthenticateUserRequest{
		Username: user,
		Pass: &PassCreds{
			Password: pass,
		},
	}
	// authentication attempt fails with no password set up
	_, err = proxy.AuthenticateWebUser(ctx, req)
	require.True(t, trace.IsAccessDenied(err))

	err = clt.UpsertPassword(user, pass)
	require.NoError(t, err)

	// success with password set up
	ws, err := proxy.AuthenticateWebUser(ctx, req)
	require.NoError(t, err)
	require.NotEqual(t, ws, "")

	web, err := tt.server.NewClientFromWebSession(ws)
	require.NoError(t, err)

	_, err = web.GetWebSessionInfo(ctx, user, ws.GetName())
	require.NoError(t, err)

	ns, err := web.ExtendWebSession(ctx, WebSessionReq{
		User:          user,
		PrevSessionID: ws.GetName(),
	})
	require.NoError(t, err)
	require.NotNil(t, ns)

	// Requesting forbidden action for user fails
	err = web.DeleteUser(ctx, user)
	require.True(t, trace.IsAccessDenied(err))

	err = clt.DeleteWebSession(ctx, user, ws.GetName())
	require.NoError(t, err)

	_, err = web.GetWebSessionInfo(ctx, user, ws.GetName())
	require.Error(t, err)

	_, err = web.ExtendWebSession(ctx, WebSessionReq{
		User:          user,
		PrevSessionID: ws.GetName(),
	})
	require.Error(t, err)
}

func TestWebSessionMultiAccessRequests(t *testing.T) {
	// Can not use t.Parallel() when changing modules
	modules.SetTestModules(t, &modules.TestModules{
		TestFeatures: modules.Features{
			ResourceAccessRequests: true,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, clt.Close()) })

	// Upsert a node to request access to
	node := &types.ServerV2{
		Kind:    types.KindNode,
		Version: types.V2,
		Metadata: types.Metadata{
			Name:      "node1",
			Namespace: apidefaults.Namespace,
		},
		Spec: types.ServerSpecV2{},
	}
	_, err = clt.UpsertNode(ctx, node)
	require.NoError(t, err)
	resourceIDs := []types.ResourceID{{
		Kind:        node.GetKind(),
		Name:        node.GetName(),
		ClusterName: "foobar",
	}}

	// Create user and roles.
	username := "user"
	password := []byte("hunter2")
	baseRoleName := services.RoleNameForUser(username)
	requestableRoleName := "requestable"
	user, err := CreateUserRoleAndRequestable(clt, username, requestableRoleName)
	require.NoError(t, err)
	err = clt.UpsertPassword(username, password)
	require.NoError(t, err)

	// Set search_as_roles, user can request this role only with a resource
	// access request.
	resourceRequestRoleName := "resource-requestable"
	resourceRequestRole := services.RoleForUser(user)
	resourceRequestRole.SetName(resourceRequestRoleName)
	err = clt.UpsertRole(ctx, resourceRequestRole)
	require.NoError(t, err)
	baseRole, err := clt.GetRole(ctx, baseRoleName)
	require.NoError(t, err)
	baseRole.SetSearchAsRoles([]string{resourceRequestRoleName})
	err = clt.UpsertRole(ctx, baseRole)
	require.NoError(t, err)

	// Create approved role request
	roleReq, err := services.NewAccessRequest(username, requestableRoleName)
	require.NoError(t, err)
	roleReq.SetState(types.RequestState_APPROVED)
	err = clt.CreateAccessRequest(ctx, roleReq)
	require.NoError(t, err)

	// Create approved resource request
	resourceReq, err := services.NewAccessRequestWithResources(username, []string{resourceRequestRoleName}, resourceIDs)
	require.NoError(t, err)
	resourceReq.SetState(types.RequestState_APPROVED)
	err = clt.CreateAccessRequest(ctx, resourceReq)
	require.NoError(t, err)

	// Create a web session and client for the user.
	proxyClient, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)
	baseWebSession, err := proxyClient.AuthenticateWebUser(ctx, AuthenticateUserRequest{
		Username: username,
		Pass: &PassCreds{
			Password: password,
		},
	})
	require.NoError(t, err)
	proxyClient.Close()
	baseWebClient, err := tt.server.NewClientFromWebSession(baseWebSession)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, baseWebClient.Close()) })

	expectRolesAndResources := func(t *testing.T, sess types.WebSession, expectRoles []string, expectResources []types.ResourceID) {
		sshCert, err := sshutils.ParseCertificate(sess.GetPub())
		require.NoError(t, err)
		gotRoles, err := services.ExtractRolesFromCert(sshCert)
		require.NoError(t, err)
		gotResources, err := services.ExtractAllowedResourcesFromCert(sshCert)
		require.NoError(t, err)
		assert.ElementsMatch(t, expectRoles, gotRoles)
		assert.ElementsMatch(t, expectResources, gotResources)
	}

	type extendSessionFunc func(*testing.T, *Client, types.WebSession) (*Client, types.WebSession)
	assumeRequest := func(request types.AccessRequest) extendSessionFunc {
		return func(t *testing.T, clt *Client, sess types.WebSession) (*Client, types.WebSession) {
			newSess, err := clt.ExtendWebSession(ctx, WebSessionReq{
				User:            username,
				PrevSessionID:   sess.GetName(),
				AccessRequestID: request.GetMetadata().Name,
			})
			require.NoError(t, err)
			newClt, err := tt.server.NewClientFromWebSession(newSess)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, newClt.Close()) })
			return newClt, newSess
		}
	}
	failToAssumeRequest := func(request types.AccessRequest) extendSessionFunc {
		return func(t *testing.T, clt *Client, sess types.WebSession) (*Client, types.WebSession) {
			_, err := clt.ExtendWebSession(ctx, WebSessionReq{
				User:            username,
				PrevSessionID:   sess.GetName(),
				AccessRequestID: request.GetMetadata().Name,
			})
			require.Error(t, err)
			return clt, sess
		}
	}
	switchBack := func(t *testing.T, clt *Client, sess types.WebSession) (*Client, types.WebSession) {
		newSess, err := clt.ExtendWebSession(ctx, WebSessionReq{
			User:          username,
			PrevSessionID: sess.GetName(),
			Switchback:    true,
		})
		require.NoError(t, err)
		newClt, err := tt.server.NewClientFromWebSession(newSess)
		require.NoError(t, err)
		return newClt, newSess
	}

	for _, tc := range []struct {
		desc            string
		steps           []extendSessionFunc
		expectRoles     []string
		expectResources []types.ResourceID
	}{
		{
			desc:        "base session",
			expectRoles: []string{baseRoleName},
		},
		{
			desc: "role request",
			steps: []extendSessionFunc{
				assumeRequest(roleReq),
			},
			expectRoles: []string{baseRoleName, requestableRoleName},
		},
		{
			desc: "resource request",
			steps: []extendSessionFunc{
				assumeRequest(resourceReq),
			},
			expectRoles:     []string{baseRoleName, resourceRequestRoleName},
			expectResources: resourceIDs,
		},
		{
			desc: "role then resource",
			steps: []extendSessionFunc{
				assumeRequest(roleReq),
				assumeRequest(resourceReq),
			},
			expectRoles:     []string{baseRoleName, requestableRoleName, resourceRequestRoleName},
			expectResources: resourceIDs,
		},
		{
			desc: "resource then role",
			steps: []extendSessionFunc{
				assumeRequest(resourceReq),
				assumeRequest(roleReq),
			},
			expectRoles:     []string{baseRoleName, requestableRoleName, resourceRequestRoleName},
			expectResources: resourceIDs,
		},
		{
			desc: "duplicates",
			steps: []extendSessionFunc{
				assumeRequest(resourceReq),
				assumeRequest(roleReq),
				// Cannot combine resource requests, this also blocks assuming
				// the same one twice.
				failToAssumeRequest(resourceReq),
				assumeRequest(roleReq),
			},
			expectRoles:     []string{baseRoleName, requestableRoleName, resourceRequestRoleName},
			expectResources: resourceIDs,
		},
		{
			desc: "switch back",
			steps: []extendSessionFunc{
				assumeRequest(roleReq),
				assumeRequest(resourceReq),
				switchBack,
			},
			expectRoles: []string{baseRoleName},
		},
	} {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			clt, sess := baseWebClient, baseWebSession
			for _, extendSession := range tc.steps {
				clt, sess = extendSession(t, clt, sess)
			}
			expectRolesAndResources(t, sess, tc.expectRoles, tc.expectResources)
		})
	}
}

func TestWebSessionWithApprovedAccessRequestAndSwitchback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	user := "user2"
	pass := []byte("abc123")

	newUser, err := CreateUserRoleAndRequestable(clt, user, "test-request-role")
	require.NoError(t, err)
	require.Len(t, newUser.GetRoles(), 1)
	require.Empty(t, cmp.Diff(newUser.GetRoles(), []string{"user:user2"}))

	proxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	// Create a user to create a web session for.
	req := AuthenticateUserRequest{
		Username: user,
		Pass: &PassCreds{
			Password: pass,
		},
	}

	err = clt.UpsertPassword(user, pass)
	require.NoError(t, err)

	ws, err := proxy.AuthenticateWebUser(ctx, req)
	require.NoError(t, err)

	web, err := tt.server.NewClientFromWebSession(ws)
	require.NoError(t, err)

	initialRole := newUser.GetRoles()[0]
	initialSession, err := web.GetWebSessionInfo(ctx, user, ws.GetName())
	require.NoError(t, err)

	// Create a approved access request.
	accessReq, err := services.NewAccessRequest(user, []string{"test-request-role"}...)
	require.NoError(t, err)

	// Set a lesser expiry date, to test switching back to default expiration later.
	accessReq.SetAccessExpiry(tt.clock.Now().Add(time.Minute * 10))
	accessReq.SetState(types.RequestState_APPROVED)

	err = clt.CreateAccessRequest(ctx, accessReq)
	require.NoError(t, err)

	sess1, err := web.ExtendWebSession(ctx, WebSessionReq{
		User:            user,
		PrevSessionID:   ws.GetName(),
		AccessRequestID: accessReq.GetMetadata().Name,
	})
	require.NoError(t, err)
	require.Equal(t, sess1.Expiry(), tt.clock.Now().Add(time.Minute*10))
	require.Equal(t, sess1.GetLoginTime(), initialSession.GetLoginTime())

	sshcert, err := sshutils.ParseCertificate(sess1.GetPub())
	require.NoError(t, err)

	// Roles extracted from cert should contain the initial role and the role assigned with access request.
	roles, err := services.ExtractRolesFromCert(sshcert)
	require.NoError(t, err)
	require.Len(t, roles, 2)

	mappedRole := map[string]string{
		roles[0]: "",
		roles[1]: "",
	}

	_, hasRole := mappedRole[initialRole]
	require.Equal(t, hasRole, true)

	_, hasRole = mappedRole["test-request-role"]
	require.Equal(t, hasRole, true)

	// certRequests extracts the active requests from a PEM encoded TLS cert.
	certRequests := func(tlsCert []byte) []string {
		cert, err := tlsca.ParseCertificatePEM(tlsCert)
		require.NoError(t, err)

		identity, err := tlsca.FromSubject(cert.Subject, cert.NotAfter)
		require.NoError(t, err)

		return identity.ActiveRequests
	}

	require.Empty(t, cmp.Diff(certRequests(sess1.GetTLSCert()), []string{accessReq.GetName()}))

	// Test switch back to default role and expiry.
	sess2, err := web.ExtendWebSession(ctx, WebSessionReq{
		User:          user,
		PrevSessionID: ws.GetName(),
		Switchback:    true,
	})
	require.NoError(t, err)
	require.Equal(t, sess2.GetExpiryTime(), initialSession.GetExpiryTime())
	require.Equal(t, sess2.GetLoginTime(), initialSession.GetLoginTime())

	sshcert, err = sshutils.ParseCertificate(sess2.GetPub())
	require.NoError(t, err)

	roles, err = services.ExtractRolesFromCert(sshcert)
	require.NoError(t, err)
	require.Empty(t, cmp.Diff(roles, []string{initialRole}))

	require.Len(t, certRequests(sess2.GetTLSCert()), 0)
}

// TestGetCertAuthority tests certificate authority permissions
func TestGetCertAuthority(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	// generate server keys for node
	nodeClt, err := tt.server.NewClient(TestIdentity{I: BuiltinRole{Username: "00000000-0000-0000-0000-000000000000", Role: types.RoleNode}})
	require.NoError(t, err)
	defer nodeClt.Close()

	// node is authorized to fetch CA without secrets
	ca, err := nodeClt.GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)
	for _, keyPair := range ca.GetActiveKeys().TLS {
		fmt.Printf("--> keyPair.Key: %v.\n", keyPair)
		require.Nil(t, keyPair.Key)
	}
	for _, keyPair := range ca.GetActiveKeys().SSH {
		require.Nil(t, keyPair.PrivateKey)
	}

	// node is not authorized to fetch CA with secrets
	_, err = nodeClt.GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, true)
	require.True(t, trace.IsAccessDenied(err))

	// non-admin users are not allowed to get access to private key material
	user, err := types.NewUser("bob")
	require.NoError(t, err)

	role := services.RoleForUser(user)
	role.SetLogins(types.Allow, []string{user.GetName()})
	err = tt.server.Auth().UpsertRole(ctx, role)
	require.NoError(t, err)

	user.AddRole(role.GetName())
	err = tt.server.Auth().UpsertUser(user)
	require.NoError(t, err)

	userClt, err := tt.server.NewClient(TestUser(user.GetName()))
	require.NoError(t, err)
	defer userClt.Close()

	// user is authorized to fetch CA without secrets
	_, err = userClt.GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)

	// user is not authorized to fetch CA with secrets
	_, err = userClt.GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, true)
	require.True(t, trace.IsAccessDenied(err))
}

func TestPluginData(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	priv, pub, err := native.GenerateKeyPair()
	require.NoError(t, err)

	// make sure we can parse the private and public key
	privateKey, err := ssh.ParseRawPrivateKey(priv)
	require.NoError(t, err)

	_, err = tlsca.MarshalPublicKeyFromPrivateKeyPEM(privateKey)
	require.NoError(t, err)

	_, _, _, _, err = ssh.ParseAuthorizedKey(pub)
	require.NoError(t, err)

	user := "user1"
	role := "some-role"
	_, err = CreateUserRoleAndRequestable(tt.server.Auth(), user, role)
	require.NoError(t, err)

	testUser := TestUser(user)
	testUser.TTL = time.Hour
	userClient, err := tt.server.NewClient(testUser)
	require.NoError(t, err)

	plugin := "my-plugin"
	_, err = CreateAccessPluginUser(context.TODO(), tt.server.Auth(), plugin)
	require.NoError(t, err)

	pluginUser := TestUser(plugin)
	pluginUser.TTL = time.Hour
	pluginClient, err := tt.server.NewClient(pluginUser)
	require.NoError(t, err)

	req, err := services.NewAccessRequest(user, role)
	require.NoError(t, err)

	require.NoError(t, userClient.CreateAccessRequest(context.TODO(), req))

	err = pluginClient.UpdatePluginData(context.TODO(), types.PluginDataUpdateParams{
		Kind:     types.KindAccessRequest,
		Resource: req.GetName(),
		Plugin:   plugin,
		Set: map[string]string{
			"foo": "bar",
		},
	})
	require.NoError(t, err)

	data, err := pluginClient.GetPluginData(context.TODO(), types.PluginDataFilter{
		Kind:     types.KindAccessRequest,
		Resource: req.GetName(),
	})
	require.NoError(t, err)
	require.Equal(t, len(data), 1)

	entry, ok := data[0].Entries()[plugin]
	require.Equal(t, ok, true)
	require.Empty(t, cmp.Diff(entry.Data, map[string]string{"foo": "bar"}))

	err = pluginClient.UpdatePluginData(context.TODO(), types.PluginDataUpdateParams{
		Kind:     types.KindAccessRequest,
		Resource: req.GetName(),
		Plugin:   plugin,
		Set: map[string]string{
			"foo":  "",
			"spam": "eggs",
		},
		Expect: map[string]string{
			"foo": "bar",
		},
	})
	require.NoError(t, err)

	data, err = pluginClient.GetPluginData(context.TODO(), types.PluginDataFilter{
		Kind:     types.KindAccessRequest,
		Resource: req.GetName(),
	})
	require.NoError(t, err)
	require.Equal(t, len(data), 1)

	entry, ok = data[0].Entries()[plugin]
	require.Equal(t, ok, true)
	require.Empty(t, cmp.Diff(entry.Data, map[string]string{"spam": "eggs"}))
}

// TestGenerateCerts tests edge cases around authorization of
// certificate generation for servers and users
func TestGenerateCerts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	srv := newTestTLSServer(t)
	priv, pub, err := native.GenerateKeyPair()
	require.NoError(t, err)

	// make sure we can parse the private and public key
	privateKey, err := ssh.ParseRawPrivateKey(priv)
	require.NoError(t, err)

	pubTLS, err := tlsca.MarshalPublicKeyFromPrivateKeyPEM(privateKey)
	require.NoError(t, err)

	_, _, _, _, err = ssh.ParseAuthorizedKey(pub)
	require.NoError(t, err)

	// generate server keys for node
	hostID := "00000000-0000-0000-0000-000000000000"
	hostClient, err := srv.NewClient(TestIdentity{I: BuiltinRole{Username: hostID, Role: types.RoleNode}})
	require.NoError(t, err)

	certs, err := hostClient.GenerateHostCerts(context.Background(),
		&proto.HostCertsRequest{
			HostID:               hostID,
			NodeName:             srv.AuthServer.ClusterName,
			Role:                 types.RoleNode,
			AdditionalPrincipals: []string{"example.com"},
			PublicSSHKey:         pub,
			PublicTLSKey:         pubTLS,
		})
	require.NoError(t, err)

	hostCert, err := sshutils.ParseCertificate(certs.SSH)
	require.NoError(t, err)
	require.Contains(t, hostCert.ValidPrincipals, "example.com")

	// sign server public keys for node
	hostID = "00000000-0000-0000-0000-000000000000"
	hostClient, err = srv.NewClient(TestIdentity{I: BuiltinRole{Username: hostID, Role: types.RoleNode}})
	require.NoError(t, err)

	certs, err = hostClient.GenerateHostCerts(context.Background(),
		&proto.HostCertsRequest{
			HostID:               hostID,
			NodeName:             srv.AuthServer.ClusterName,
			Role:                 types.RoleNode,
			AdditionalPrincipals: []string{"example.com"},
			PublicSSHKey:         pub,
			PublicTLSKey:         pubTLS,
		})
	require.NoError(t, err)

	hostCert, err = sshutils.ParseCertificate(certs.SSH)
	require.NoError(t, err)
	require.Contains(t, hostCert.ValidPrincipals, "example.com")

	t.Run("HostClients", func(t *testing.T) {
		// attempt to elevate privileges by getting admin role in the certificate
		_, err = hostClient.GenerateHostCerts(context.Background(),
			&proto.HostCertsRequest{
				HostID:       hostID,
				NodeName:     srv.AuthServer.ClusterName,
				Role:         types.RoleAdmin,
				PublicSSHKey: pub,
				PublicTLSKey: pubTLS,
			})
		require.True(t, trace.IsAccessDenied(err))

		// attempt to get certificate for different host id
		_, err = hostClient.GenerateHostCerts(context.Background(),
			&proto.HostCertsRequest{
				HostID:       "some-other-host-id",
				NodeName:     srv.AuthServer.ClusterName,
				Role:         types.RoleNode,
				PublicSSHKey: pub,
				PublicTLSKey: pubTLS,
			})
		require.True(t, trace.IsAccessDenied(err))
	})

	user1, userRole, err := CreateUserAndRole(srv.Auth(), "user1", []string{"user1"})
	require.NoError(t, err)

	user2, userRole2, err := CreateUserAndRole(srv.Auth(), "user2", []string{"user2"})
	require.NoError(t, err)

	t.Run("Nop", func(t *testing.T) {
		// unauthenticated client should NOT be able to generate a user cert without auth
		nopClient, err := srv.NewClient(TestNop())
		require.NoError(t, err)

		_, err = nopClient.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey: pub,
			Username:  user1.GetName(),
			Expires:   time.Now().Add(time.Hour).UTC(),
			Format:    constants.CertificateFormatStandard,
		})
		require.Error(t, err)
		require.True(t, trace.IsAccessDenied(err), err.Error())
	})

	testUser2 := TestUser(user2.GetName())
	testUser2.TTL = time.Hour
	userClient2, err := srv.NewClient(testUser2)
	require.NoError(t, err)

	t.Run("ImpersonateDeny", func(t *testing.T) {
		// User can't generate certificates for another user by default
		_, err = userClient2.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey: pub,
			Username:  user1.GetName(),
			Expires:   time.Now().Add(time.Hour).UTC(),
			Format:    constants.CertificateFormatStandard,
		})
		require.Error(t, err)
		require.True(t, trace.IsAccessDenied(err))
	})

	parseCert := func(sshCert []byte) (*ssh.Certificate, time.Duration) {
		parsedCert, err := sshutils.ParseCertificate(sshCert)
		require.NoError(t, err)
		validBefore := time.Unix(int64(parsedCert.ValidBefore), 0)
		return parsedCert, time.Until(validBefore)
	}

	clock := srv.Auth().GetClock()
	t.Run("ImpersonateAllow", func(t *testing.T) {
		// Super impersonator impersonate anyone and login as root
		maxSessionTTL := 300 * time.Hour
		superImpersonatorRole, err := types.NewRoleV3("superimpersonator", types.RoleSpecV5{
			Options: types.RoleOptions{
				MaxSessionTTL: types.Duration(maxSessionTTL),
			},
			Allow: types.RoleConditions{
				Logins: []string{"root"},
				Impersonate: &types.ImpersonateConditions{
					Users: []string{types.Wildcard},
					Roles: []string{types.Wildcard},
				},
				Rules: []types.Rule{},
			},
		})
		require.NoError(t, err)
		superImpersonator, err := CreateUser(srv.Auth(), "superimpersonator", superImpersonatorRole)
		require.NoError(t, err)

		// Impersonator can generate certificates for super impersonator
		role, err := types.NewRoleV3("impersonate", types.RoleSpecV5{
			Allow: types.RoleConditions{
				Logins: []string{superImpersonator.GetName()},
				Impersonate: &types.ImpersonateConditions{
					Users: []string{superImpersonator.GetName()},
					Roles: []string{superImpersonatorRole.GetName()},
				},
			},
		})
		require.NoError(t, err)
		impersonator, err := CreateUser(srv.Auth(), "impersonator", role)
		require.NoError(t, err)

		iUser := TestUser(impersonator.GetName())
		iUser.TTL = time.Hour
		iClient, err := srv.NewClient(iUser)
		require.NoError(t, err)

		// can impersonate super impersonator and request certs
		// longer than their own TTL, but not exceeding super impersonator's max session ttl
		userCerts, err := iClient.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey: pub,
			Username:  superImpersonator.GetName(),
			Expires:   clock.Now().Add(1000 * time.Hour).UTC(),
			Format:    constants.CertificateFormatStandard,
		})
		require.NoError(t, err)

		_, diff := parseCert(userCerts.SSH)
		require.Less(t, int64(diff), int64(iUser.TTL))

		tlsCert, err := tlsca.ParseCertificatePEM(userCerts.TLS)
		require.NoError(t, err)
		identity, err := tlsca.FromSubject(tlsCert.Subject, tlsCert.NotAfter)
		require.NoError(t, err)

		// Because the original request has maxed out the possible max
		// session TTL, it will be adjusted to exactly the value
		require.Equal(t, identity.Expires.Sub(clock.Now()), maxSessionTTL)
		require.Equal(t, impersonator.GetName(), identity.Impersonator)
		require.Equal(t, superImpersonator.GetName(), identity.Username)

		// impersonator can't impersonate user1
		_, err = iClient.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey: pub,
			Username:  user1.GetName(),
			Expires:   clock.Now().Add(time.Hour).UTC(),
			Format:    constants.CertificateFormatStandard,
		})
		require.Error(t, err)
		require.IsType(t, &trace.AccessDeniedError{}, trace.Unwrap(err))

		_, privateKeyPEM, err := utils.MarshalPrivateKey(privateKey.(crypto.Signer))
		require.NoError(t, err)

		clientCert, err := tls.X509KeyPair(userCerts.TLS, privateKeyPEM)
		require.NoError(t, err)

		// client that uses impersonated certificate can't impersonate other users
		// although super impersonator's roles allow it
		impersonatedClient := srv.NewClientWithCert(clientCert)
		_, err = impersonatedClient.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey: pub,
			Username:  user1.GetName(),
			Expires:   time.Now().Add(time.Hour).UTC(),
			Format:    constants.CertificateFormatStandard,
		})
		require.Error(t, err)
		require.IsType(t, &trace.AccessDeniedError{}, trace.Unwrap(err))
		require.Contains(t, err.Error(), "impersonated user can not impersonate anyone else")

		// but can renew their own cert, for example set route to cluster
		rc, err := types.NewRemoteCluster("cluster-remote")
		require.NoError(t, err)
		err = srv.Auth().CreateRemoteCluster(rc)
		require.NoError(t, err)

		userCerts, err = impersonatedClient.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey:      pub,
			Username:       superImpersonator.GetName(),
			Expires:        clock.Now().Add(time.Hour).UTC(),
			Format:         constants.CertificateFormatStandard,
			RouteToCluster: rc.GetName(),
		})
		require.NoError(t, err)
		// Make sure impersonator was not lost in the renewed cert
		tlsCert, err = tlsca.ParseCertificatePEM(userCerts.TLS)
		require.NoError(t, err)
		identity, err = tlsca.FromSubject(tlsCert.Subject, tlsCert.NotAfter)
		require.NoError(t, err)
		require.Equal(t, identity.Expires.Sub(clock.Now()), time.Hour)
		require.Equal(t, impersonator.GetName(), identity.Impersonator)
		require.Equal(t, superImpersonator.GetName(), identity.Username)
	})

	t.Run("Renew", func(t *testing.T) {
		testUser2 := TestUser(user2.GetName())
		testUser2.TTL = time.Hour
		userClient2, err := srv.NewClient(testUser2)
		require.NoError(t, err)

		rc1, err := types.NewRemoteCluster("cluster1")
		require.NoError(t, err)
		err = srv.Auth().CreateRemoteCluster(rc1)
		require.NoError(t, err)

		// User can renew their certificates, however the TTL will be limited
		// to the TTL of their session for both SSH and x509 certs and
		// that route to cluster will be encoded in the cert metadata
		userCerts, err := userClient2.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey:      pub,
			Username:       user2.GetName(),
			Expires:        time.Now().Add(100 * time.Hour).UTC(),
			Format:         constants.CertificateFormatStandard,
			RouteToCluster: rc1.GetName(),
		})
		require.NoError(t, err)

		_, diff := parseCert(userCerts.SSH)
		require.Less(t, int64(diff), int64(testUser2.TTL))

		tlsCert, err := tlsca.ParseCertificatePEM(userCerts.TLS)
		require.NoError(t, err)
		identity, err := tlsca.FromSubject(tlsCert.Subject, tlsCert.NotAfter)
		require.NoError(t, err)
		require.True(t, identity.Expires.Before(time.Now().Add(testUser2.TTL)))
		require.Equal(t, identity.RouteToCluster, rc1.GetName())
	})

	t.Run("Admin", func(t *testing.T) {
		// Admin should be allowed to generate certs with TTL longer than max.
		adminClient, err := srv.NewClient(TestAdmin())
		require.NoError(t, err)

		userCerts, err := adminClient.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey: pub,
			Username:  user1.GetName(),
			Expires:   time.Now().Add(40 * time.Hour).UTC(),
			Format:    constants.CertificateFormatStandard,
		})
		require.NoError(t, err)

		parsedCert, diff := parseCert(userCerts.SSH)
		require.Less(t, int64(apidefaults.MaxCertDuration), int64(diff))

		// user should have agent forwarding (default setting)
		require.Contains(t, parsedCert.Extensions, teleport.CertExtensionPermitAgentForwarding)

		// user should not have X11 forwarding (default setting)
		require.NotContains(t, parsedCert.Extensions, teleport.CertExtensionPermitX11Forwarding)

		// now update role to permit agent and X11 forwarding
		roleOptions := userRole.GetOptions()
		roleOptions.ForwardAgent = types.NewBool(true)
		roleOptions.PermitX11Forwarding = types.NewBool(true)
		userRole.SetOptions(roleOptions)
		err = srv.Auth().UpsertRole(ctx, userRole)
		require.NoError(t, err)

		userCerts, err = adminClient.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey: pub,
			Username:  user1.GetName(),
			Expires:   time.Now().Add(1 * time.Hour).UTC(),
			Format:    constants.CertificateFormatStandard,
		})
		require.NoError(t, err)
		parsedCert, _ = parseCert(userCerts.SSH)

		// user should get agent forwarding
		require.Contains(t, parsedCert.Extensions, teleport.CertExtensionPermitAgentForwarding)

		// user should get X11 forwarding
		require.Contains(t, parsedCert.Extensions, teleport.CertExtensionPermitX11Forwarding)

		// apply HTTP Auth to generate user cert:
		userCerts, err = adminClient.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey: pub,
			Username:  user1.GetName(),
			Expires:   time.Now().Add(time.Hour).UTC(),
			Format:    constants.CertificateFormatStandard,
		})
		require.NoError(t, err)

		_, _, _, _, err = ssh.ParseAuthorizedKey(userCerts.SSH)
		require.NoError(t, err)
	})

	t.Run("DenyLeaf", func(t *testing.T) {
		// User can't generate certificates for an unknown leaf cluster.
		_, err = userClient2.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey:      pub,
			Username:       user2.GetName(),
			Expires:        time.Now().Add(100 * time.Hour).UTC(),
			Format:         constants.CertificateFormatStandard,
			RouteToCluster: "unknown_cluster",
		})
		require.Error(t, err)

		rc2, err := types.NewRemoteCluster("cluster2")
		require.NoError(t, err)
		meta := rc2.GetMetadata()
		meta.Labels = map[string]string{"env": "prod"}
		rc2.SetMetadata(meta)
		err = srv.Auth().CreateRemoteCluster(rc2)
		require.NoError(t, err)

		// User can't generate certificates for leaf cluster they don't have access
		// to due to labels.
		_, err = userClient2.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey:      pub,
			Username:       user2.GetName(),
			Expires:        time.Now().Add(100 * time.Hour).UTC(),
			Format:         constants.CertificateFormatStandard,
			RouteToCluster: rc2.GetName(),
		})
		require.Error(t, err)

		userRole2.SetClusterLabels(types.Allow, types.Labels{"env": apiutils.Strings{"prod"}})
		err = srv.Auth().UpsertRole(ctx, userRole2)
		require.NoError(t, err)

		// User can generate certificates for leaf cluster they do have access to.
		userCerts, err := userClient2.GenerateUserCerts(ctx, proto.UserCertsRequest{
			PublicKey:      pub,
			Username:       user2.GetName(),
			Expires:        time.Now().Add(100 * time.Hour).UTC(),
			Format:         constants.CertificateFormatStandard,
			RouteToCluster: rc2.GetName(),
		})
		require.NoError(t, err)

		tlsCert, err := tlsca.ParseCertificatePEM(userCerts.TLS)
		require.NoError(t, err)
		identity, err := tlsca.FromSubject(tlsCert.Subject, tlsCert.NotAfter)
		require.NoError(t, err)
		require.Equal(t, identity.RouteToCluster, rc2.GetName())
	})
}

// TestGenerateAppToken checks the identity of the caller and makes sure only
// certain roles can request JWT tokens.
func TestGenerateAppToken(t *testing.T) {
	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	authClient, err := tt.server.NewClient(TestBuiltin(types.RoleAdmin))
	require.NoError(t, err)

	ca, err := authClient.GetCertAuthority(context.Background(), types.CertAuthID{
		Type:       types.JWTSigner,
		DomainName: tt.server.ClusterName(),
	}, true)
	require.NoError(t, err)

	signer, err := tt.server.AuthServer.AuthServer.GetKeyStore().GetJWTSigner(ca)
	require.NoError(t, err)
	key, err := services.GetJWTSigner(signer, ca.GetClusterName(), tt.clock)
	require.NoError(t, err)

	tests := []struct {
		inMachineRole types.SystemRole
		inComment     string
		outError      bool
	}{
		{
			inMachineRole: types.RoleNode,
			inComment:     "nodes should not have the ability to generate tokens",
			outError:      true,
		},
		{
			inMachineRole: types.RoleProxy,
			inComment:     "proxies should not have the ability to generate tokens",
			outError:      true,
		},
		{
			inMachineRole: types.RoleApp,
			inComment:     "only apps should have the ability to generate tokens",
			outError:      false,
		},
	}
	for _, ts := range tests {
		client, err := tt.server.NewClient(TestBuiltin(ts.inMachineRole))
		require.NoError(t, err, ts.inComment)

		token, err := client.GenerateAppToken(
			context.Background(),
			types.GenerateAppTokenRequest{
				Username: "foo@example.com",
				Roles:    []string{"bar", "baz"},
				URI:      "http://localhost:8080",
				Expires:  tt.clock.Now().Add(1 * time.Minute),
			})
		require.Equal(t, err != nil, ts.outError, ts.inComment)
		if !ts.outError {
			claims, err := key.Verify(jwt.VerifyParams{
				Username: "foo@example.com",
				RawToken: token,
				URI:      "http://localhost:8080",
			})
			require.NoError(t, err, ts.inComment)
			require.Equal(t, claims.Username, "foo@example.com", ts.inComment)
			require.Empty(t, cmp.Diff(claims.Roles, []string{"bar", "baz"}), ts.inComment)
		}
	}
}

// TestCertificateFormat makes sure that certificates are generated with the
// correct format.
func TestCertificateFormat(t *testing.T) {
	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	priv, pub, err := native.GenerateKeyPair()
	require.NoError(t, err)

	// make sure we can parse the private and public key
	_, err = ssh.ParsePrivateKey(priv)
	require.NoError(t, err)
	_, _, _, _, err = ssh.ParseAuthorizedKey(pub)
	require.NoError(t, err)

	// use admin client to create user and role
	user, userRole, err := CreateUserAndRole(tt.server.Auth(), "user", []string{"user"})
	require.NoError(t, err)

	pass := []byte("very secure password")
	err = tt.server.Auth().UpsertPassword(user.GetName(), pass)
	require.NoError(t, err)

	tests := []struct {
		inRoleCertificateFormat   string
		inClientCertificateFormat string
		outCertContainsRole       bool
	}{
		// 0 - take whatever the role has
		{
			teleport.CertificateFormatOldSSH,
			teleport.CertificateFormatUnspecified,
			false,
		},
		// 1 - override the role
		{
			teleport.CertificateFormatOldSSH,
			constants.CertificateFormatStandard,
			true,
		},
	}

	for _, ts := range tests {
		roleOptions := userRole.GetOptions()
		roleOptions.CertificateFormat = ts.inRoleCertificateFormat
		userRole.SetOptions(roleOptions)
		err := tt.server.Auth().UpsertRole(ctx, userRole)
		require.NoError(t, err)

		proxyClient, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
		require.NoError(t, err)

		// authentication attempt fails with password auth only
		re, err := proxyClient.AuthenticateSSHUser(ctx, AuthenticateSSHRequest{
			AuthenticateUserRequest: AuthenticateUserRequest{
				Username: user.GetName(),
				Pass: &PassCreds{
					Password: pass,
				},
			},
			CompatibilityMode: ts.inClientCertificateFormat,
			TTL:               apidefaults.CertDuration,
			PublicKey:         pub,
		})
		require.NoError(t, err)

		parsedCert, err := sshutils.ParseCertificate(re.Cert)
		require.NoError(t, err)

		_, ok := parsedCert.Extensions[teleport.CertExtensionTeleportRoles]
		require.Equal(t, ok, ts.outCertContainsRole)
	}
}

// TestClusterConfigContext checks that the cluster configuration gets passed
// along in the context and permissions get updated accordingly.
func TestClusterConfigContext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	proxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	_, pub, err := native.GenerateKeyPair()
	require.NoError(t, err)

	// try and generate a host cert, this should fail because we are recording
	// at the nodes not at the proxy
	_, err = proxy.GenerateHostCert(pub,
		"a", "b", nil,
		"localhost", types.RoleProxy, 0)
	require.True(t, trace.IsAccessDenied(err))

	// update cluster config to record at the proxy
	recConfig, err := types.NewSessionRecordingConfigFromConfigFile(types.SessionRecordingConfigSpecV2{
		Mode: types.RecordAtProxy,
	})
	require.NoError(t, err)
	err = tt.server.Auth().SetSessionRecordingConfig(ctx, recConfig)
	require.NoError(t, err)

	// try and generate a host cert, now the proxy should be able to generate a
	// host cert because it's in recording mode.
	_, err = proxy.GenerateHostCert(pub,
		"a", "b", nil,
		"localhost", types.RoleProxy, 0)
	require.NoError(t, err)
}

// TestAuthenticateWebUserOTP tests web authentication flow for password + OTP
func TestAuthenticateWebUserOTP(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	user := "ws-test"
	pass := []byte("ws-abc123")
	rawSecret := "def456"
	otpSecret := base32.StdEncoding.EncodeToString([]byte(rawSecret))

	_, _, err = CreateUserAndRole(clt, user, []string{user})
	require.NoError(t, err)

	err = tt.server.Auth().UpsertPassword(user, pass)
	require.NoError(t, err)

	dev, err := services.NewTOTPDevice("otp", otpSecret, tt.clock.Now())
	require.NoError(t, err)
	err = tt.server.Auth().UpsertMFADevice(ctx, user, dev)
	require.NoError(t, err)

	// create a valid otp token
	validToken, err := totp.GenerateCode(otpSecret, tt.clock.Now())
	require.NoError(t, err)

	proxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	authPreference, err := types.NewAuthPreference(types.AuthPreferenceSpecV2{
		Type:         constants.Local,
		SecondFactor: constants.SecondFactorOTP,
	})
	require.NoError(t, err)
	err = tt.server.Auth().SetAuthPreference(ctx, authPreference)
	require.NoError(t, err)

	// authentication attempt fails with wrong password
	_, err = proxy.AuthenticateWebUser(ctx, AuthenticateUserRequest{
		Username: user,
		OTP:      &OTPCreds{Password: []byte("wrong123"), Token: validToken},
	})
	require.True(t, trace.IsAccessDenied(err))

	// authentication attempt fails with wrong otp
	_, err = proxy.AuthenticateWebUser(ctx, AuthenticateUserRequest{
		Username: user,
		OTP:      &OTPCreds{Password: pass, Token: "wrong123"},
	})
	require.True(t, trace.IsAccessDenied(err))

	// authentication attempt fails with password auth only
	_, err = proxy.AuthenticateWebUser(ctx, AuthenticateUserRequest{
		Username: user,
		Pass: &PassCreds{
			Password: pass,
		},
	})
	require.True(t, trace.IsAccessDenied(err))

	// authentication succeeds
	ws, err := proxy.AuthenticateWebUser(ctx, AuthenticateUserRequest{
		Username: user,
		OTP:      &OTPCreds{Password: pass, Token: validToken},
	})
	require.NoError(t, err)

	userClient, err := tt.server.NewClientFromWebSession(ws)
	require.NoError(t, err)

	_, err = userClient.GetWebSessionInfo(ctx, user, ws.GetName())
	require.NoError(t, err)

	err = clt.DeleteWebSession(ctx, user, ws.GetName())
	require.NoError(t, err)

	_, err = userClient.GetWebSessionInfo(ctx, user, ws.GetName())
	require.Error(t, err)
}

// TestLoginAttempts makes sure the login attempt counter is incremented and
// reset correctly.
func TestLoginAttempts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	user := "user1"
	pass := []byte("abc123")

	_, _, err = CreateUserAndRole(clt, user, []string{user})
	require.NoError(t, err)

	proxy, err := tt.server.NewClient(TestBuiltin(types.RoleProxy))
	require.NoError(t, err)

	err = clt.UpsertPassword(user, pass)
	require.NoError(t, err)

	req := AuthenticateUserRequest{
		Username: user,
		Pass: &PassCreds{
			Password: []byte("bad pass"),
		},
	}
	// authentication attempt fails with bad password
	_, err = proxy.AuthenticateWebUser(ctx, req)
	require.True(t, trace.IsAccessDenied(err))

	// creates first failed login attempt
	loginAttempts, err := tt.server.Auth().GetUserLoginAttempts(user)
	require.NoError(t, err)
	require.Len(t, loginAttempts, 1)

	// try second time with wrong pass
	req.Pass.Password = pass
	_, err = proxy.AuthenticateWebUser(ctx, req)
	require.NoError(t, err)

	// clears all failed attempts after success
	loginAttempts, err = tt.server.Auth().GetUserLoginAttempts(user)
	require.NoError(t, err)
	require.Len(t, loginAttempts, 0)
}

func TestChangeUserAuthenticationSettings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	authPref, err := types.NewAuthPreference(types.AuthPreferenceSpecV2{
		AllowLocalAuth: types.NewBoolOption(true),
	})
	require.NoError(t, err)

	err = tt.server.Auth().SetAuthPreference(ctx, authPref)
	require.NoError(t, err)

	authPreference, err := types.NewAuthPreference(types.AuthPreferenceSpecV2{
		Type:         constants.Local,
		SecondFactor: constants.SecondFactorOTP,
	})
	require.NoError(t, err)

	err = tt.server.Auth().SetAuthPreference(ctx, authPreference)
	require.NoError(t, err)

	username := "user1"
	// Create a local user.
	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	_, _, err = CreateUserAndRole(clt, username, []string{"role1"})
	require.NoError(t, err)

	token, err := tt.server.Auth().CreateResetPasswordToken(ctx, CreateUserTokenRequest{
		Name: username,
		TTL:  time.Hour,
	})
	require.NoError(t, err)

	res, err := tt.server.Auth().CreateRegisterChallenge(ctx, &proto.CreateRegisterChallengeRequest{
		TokenID:    token.GetName(),
		DeviceType: proto.DeviceType_DEVICE_TYPE_TOTP,
	})
	require.NoError(t, err)

	otpToken, err := totp.GenerateCode(res.GetTOTP().GetSecret(), tt.server.Clock().Now())
	require.NoError(t, err)

	_, err = tt.server.Auth().ChangeUserAuthentication(ctx, &proto.ChangeUserAuthenticationRequest{
		TokenID:     token.GetName(),
		NewPassword: []byte("qweqweqwe"),
		NewMFARegisterResponse: &proto.MFARegisterResponse{Response: &proto.MFARegisterResponse_TOTP{
			TOTP: &proto.TOTPRegisterResponse{Code: otpToken},
		}},
	})
	require.NoError(t, err)
}

// TestLoginNoLocalAuth makes sure that logins for local accounts can not be
// performed when local auth is disabled.
func TestLoginNoLocalAuth(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	user := "foo"
	pass := []byte("barbaz")

	// Create a local user.
	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)
	_, _, err = CreateUserAndRole(clt, user, []string{user})
	require.NoError(t, err)
	err = clt.UpsertPassword(user, pass)
	require.NoError(t, err)

	// Set auth preference to disallow local auth.
	authPref, err := types.NewAuthPreference(types.AuthPreferenceSpecV2{
		AllowLocalAuth: types.NewBoolOption(false),
	})
	require.NoError(t, err)
	err = tt.server.Auth().SetAuthPreference(ctx, authPref)
	require.NoError(t, err)

	// Make sure access is denied for web login.
	_, err = tt.server.Auth().AuthenticateWebUser(ctx, AuthenticateUserRequest{
		Username: user,
		Pass: &PassCreds{
			Password: pass,
		},
	})
	require.True(t, trace.IsAccessDenied(err))

	// Make sure access is denied for SSH login.
	_, pub, err := native.GenerateKeyPair()
	require.NoError(t, err)
	_, err = tt.server.Auth().AuthenticateSSHUser(ctx, AuthenticateSSHRequest{
		AuthenticateUserRequest: AuthenticateUserRequest{
			Username: user,
			Pass: &PassCreds{
				Password: pass,
			},
		},
		PublicKey: pub,
	})
	require.True(t, trace.IsAccessDenied(err))
}

// TestCipherSuites makes sure that clients with invalid cipher suites can
// not connect.
func TestCipherSuites(t *testing.T) {
	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	otherServer, err := tt.server.AuthServer.NewTestTLSServer()
	require.NoError(t, err)
	defer otherServer.Close()

	// Create a client with ciphersuites that the server does not support.
	tlsConfig, err := tt.server.ClientTLSConfig(TestNop())
	require.NoError(t, err)
	tlsConfig.CipherSuites = []uint16{
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	}

	addrs := []string{
		otherServer.Addr().String(),
		tt.server.Addr().String(),
	}
	client, err := NewClient(client.Config{
		Addrs: addrs,
		Credentials: []client.Credentials{
			client.LoadTLS(tlsConfig),
		},
		CircuitBreakerConfig: breaker.NoopBreakerConfig(),
	})
	require.NoError(t, err)

	// Requests should fail.
	_, err = client.GetClusterName()
	require.Error(t, err)
}

// TestTLSFailover tests HTTP client failover between two tls servers
func TestTLSFailover(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	otherServer, err := tt.server.AuthServer.NewTestTLSServer()
	require.NoError(t, err)
	defer otherServer.Close()

	tlsConfig, err := tt.server.ClientTLSConfig(TestNop())
	require.NoError(t, err)

	addrs := []string{
		otherServer.Addr().String(),
		tt.server.Addr().String(),
	}
	client, err := NewClient(client.Config{
		Addrs: addrs,
		Credentials: []client.Credentials{
			client.LoadTLS(tlsConfig),
		},
		CircuitBreakerConfig: breaker.NoopBreakerConfig(),
	})
	require.NoError(t, err)

	// couple of runs to get enough connections
	for i := 0; i < 4; i++ {
		_, err = client.Get(ctx, client.Endpoint("not", "exist"), url.Values{})
		require.True(t, trace.IsNotFound(err))
	}

	// stop the server to get response
	err = otherServer.Stop()
	require.NoError(t, err)

	// client detects closed sockets and reconnect to the backup server
	for i := 0; i < 4; i++ {
		_, err = client.Get(ctx, client.Endpoint("not", "exist"), url.Values{})
		require.True(t, trace.IsNotFound(err))
	}
}

// TestRegisterCAPin makes sure that registration only works with a valid
// CA pin.
func TestRegisterCAPin(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	// Generate a token to use.
	token, err := tt.server.AuthServer.AuthServer.GenerateToken(ctx, &proto.GenerateTokenRequest{
		Roles: types.SystemRoles{
			types.RoleProxy,
		},
		TTL: proto.Duration(time.Hour),
	})
	require.NoError(t, err)

	// Generate public and private keys for node.
	priv, pub, err := native.GenerateKeyPair()
	require.NoError(t, err)
	privateKey, err := ssh.ParseRawPrivateKey(priv)
	require.NoError(t, err)
	pubTLS, err := tlsca.MarshalPublicKeyFromPrivateKeyPEM(privateKey)
	require.NoError(t, err)

	// Calculate what CA pin should be.
	localCAResponse, err := tt.server.AuthServer.AuthServer.GetClusterCACert(ctx)
	require.NoError(t, err)
	caPins, err := tlsca.CalculatePins(localCAResponse.TLSCA)
	require.NoError(t, err)
	require.Len(t, caPins, 1)
	caPin := caPins[0]

	// Attempt to register with valid CA pin, should work.
	_, err = Register(RegisterParams{
		Servers: []utils.NetAddr{utils.FromAddr(tt.server.Addr())},
		Token:   token,
		ID: IdentityID{
			HostUUID: "once",
			NodeName: "node-name",
			Role:     types.RoleProxy,
		},
		AdditionalPrincipals: []string{"example.com"},
		PublicSSHKey:         pub,
		PublicTLSKey:         pubTLS,
		CAPins:               []string{caPin},
		Clock:                tt.clock,
	})
	require.NoError(t, err)

	// Attempt to register with multiple CA pins where the auth server only
	// matches one, should work.
	_, err = Register(RegisterParams{
		Servers: []utils.NetAddr{utils.FromAddr(tt.server.Addr())},
		Token:   token,
		ID: IdentityID{
			HostUUID: "once",
			NodeName: "node-name",
			Role:     types.RoleProxy,
		},
		AdditionalPrincipals: []string{"example.com"},
		PublicSSHKey:         pub,
		PublicTLSKey:         pubTLS,
		CAPins:               []string{"sha256:123", caPin},
		Clock:                tt.clock,
	})
	require.NoError(t, err)

	// Attempt to register with invalid CA pin, should fail.
	_, err = Register(RegisterParams{
		Servers: []utils.NetAddr{utils.FromAddr(tt.server.Addr())},
		Token:   token,
		ID: IdentityID{
			HostUUID: "once",
			NodeName: "node-name",
			Role:     types.RoleProxy,
		},
		AdditionalPrincipals: []string{"example.com"},
		PublicSSHKey:         pub,
		PublicTLSKey:         pubTLS,
		CAPins:               []string{"sha256:123"},
		Clock:                tt.clock,
	})
	require.Error(t, err)

	// Attempt to register with multiple invalid CA pins, should fail.
	_, err = Register(RegisterParams{
		Servers: []utils.NetAddr{utils.FromAddr(tt.server.Addr())},
		Token:   token,
		ID: IdentityID{
			HostUUID: "once",
			NodeName: "node-name",
			Role:     types.RoleProxy,
		},
		AdditionalPrincipals: []string{"example.com"},
		PublicSSHKey:         pub,
		PublicTLSKey:         pubTLS,
		CAPins:               []string{"sha256:123", "sha256:456"},
		Clock:                tt.clock,
	})
	require.Error(t, err)

	// Add another cert to the CA (dupe the current one for simplicity)
	hostCA, err := tt.server.AuthServer.AuthServer.GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.AuthServer.ClusterName,
		Type:       types.HostCA,
	}, true)
	require.NoError(t, err)
	activeKeys := hostCA.GetActiveKeys()
	activeKeys.TLS = append(activeKeys.TLS, activeKeys.TLS...)
	hostCA.SetActiveKeys(activeKeys)
	err = tt.server.AuthServer.AuthServer.UpsertCertAuthority(hostCA)
	require.NoError(t, err)

	// Calculate what CA pins should be.
	localCAResponse, err = tt.server.AuthServer.AuthServer.GetClusterCACert(ctx)
	require.NoError(t, err)
	caPins, err = tlsca.CalculatePins(localCAResponse.TLSCA)
	require.NoError(t, err)
	require.Len(t, caPins, 2)

	// Attempt to register with multiple CA pins, should work
	_, err = Register(RegisterParams{
		Servers: []utils.NetAddr{utils.FromAddr(tt.server.Addr())},
		Token:   token,
		ID: IdentityID{
			HostUUID: "once",
			NodeName: "node-name",
			Role:     types.RoleProxy,
		},
		AdditionalPrincipals: []string{"example.com"},
		PublicSSHKey:         pub,
		PublicTLSKey:         pubTLS,
		CAPins:               caPins,
		Clock:                tt.clock,
	})
	require.NoError(t, err)
}

// TestRegisterCAPath makes sure registration only works with a valid CA
// file on disk.
func TestRegisterCAPath(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	// Generate a token to use.
	token, err := tt.server.AuthServer.AuthServer.GenerateToken(ctx, &proto.GenerateTokenRequest{
		Roles: types.SystemRoles{
			types.RoleProxy,
		},
		TTL: proto.Duration(time.Hour),
	})
	require.NoError(t, err)

	// Generate public and private keys for node.
	priv, pub, err := native.GenerateKeyPair()
	require.NoError(t, err)
	privateKey, err := ssh.ParseRawPrivateKey(priv)
	require.NoError(t, err)
	pubTLS, err := tlsca.MarshalPublicKeyFromPrivateKeyPEM(privateKey)
	require.NoError(t, err)

	// Attempt to register with nothing at the CA path, should work.
	_, err = Register(RegisterParams{
		Servers: []utils.NetAddr{utils.FromAddr(tt.server.Addr())},
		Token:   token,
		ID: IdentityID{
			HostUUID: "once",
			NodeName: "node-name",
			Role:     types.RoleProxy,
		},
		AdditionalPrincipals: []string{"example.com"},
		PublicSSHKey:         pub,
		PublicTLSKey:         pubTLS,
		Clock:                tt.clock,
	})
	require.NoError(t, err)

	// Extract the root CA public key and write it out to the data dir.
	hostCA, err := tt.server.AuthServer.AuthServer.GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.AuthServer.ClusterName,
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)
	certs := services.GetTLSCerts(hostCA)
	require.Len(t, certs, 1)
	certPem := certs[0]
	caPath := filepath.Join(tt.dataDir, defaults.CACertFile)
	err = os.WriteFile(caPath, certPem, teleport.FileMaskOwnerOnly)
	require.NoError(t, err)

	// Attempt to register with valid CA path, should work.
	_, err = Register(RegisterParams{
		Servers: []utils.NetAddr{utils.FromAddr(tt.server.Addr())},
		Token:   token,
		ID: IdentityID{
			HostUUID: "once",
			NodeName: "node-name",
			Role:     types.RoleProxy,
		},
		AdditionalPrincipals: []string{"example.com"},
		PublicSSHKey:         pub,
		PublicTLSKey:         pubTLS,
		CAPath:               caPath,
		Clock:                tt.clock,
	})
	require.NoError(t, err)
}

// TestClusterAlertAccessControls verifies expected behaviors of cluster alert
// access controls.
func TestClusterAlertAccessControls(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tt := setupAuthContext(ctx, t)

	alert1, err := types.NewClusterAlert("alert-1", "some msg")
	require.NoError(t, err)

	alert2, err := types.NewClusterAlert("alert-2", "other msg")
	require.NoError(t, err)

	// set one of the two alerts to be viewable by all users
	alert2.Metadata.Labels = map[string]string{
		types.AlertPermitAll: "yes",
	}

	adminClt, err := tt.server.NewClient(TestBuiltin(types.RoleAdmin))
	require.NoError(t, err)
	defer adminClt.Close()

	err = adminClt.UpsertClusterAlert(ctx, alert1)
	require.NoError(t, err)

	err = adminClt.UpsertClusterAlert(ctx, alert2)
	require.NoError(t, err)

	// verify that admin client can see all alerts
	alerts, err := adminClt.GetClusterAlerts(ctx, types.GetClusterAlertsRequest{})
	require.NoError(t, err)
	require.Len(t, alerts, 2)

	// verify that some other client with no alert-specific permissions can
	// see the "permit-all" subset of alerts (using role node here, but any
	// role with no special provisions for alerts should be equivalent)
	otherClt, err := tt.server.NewClient(TestBuiltin(types.RoleNode))
	require.NoError(t, err)
	defer otherClt.Close()

	alerts, err = otherClt.GetClusterAlerts(ctx, types.GetClusterAlertsRequest{})
	require.NoError(t, err)
	require.Len(t, alerts, 1)

	// verify that we still reject unauthenticated clients
	nopClt, err := tt.server.NewClient(TestBuiltin(types.RoleNop))
	require.NoError(t, err)
	defer nopClt.Close()

	_, err = nopClt.GetClusterAlerts(ctx, types.GetClusterAlertsRequest{})
	require.True(t, trace.IsAccessDenied(err))
}

// TestEventsNodePresence tests streaming node presence API -
// announcing node and keeping node alive
func TestEventsNodePresence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	node := &types.ServerV2{
		Kind:    types.KindNode,
		Version: types.V2,
		Metadata: types.Metadata{
			Name:      "node1",
			Namespace: apidefaults.Namespace,
		},
		Spec: types.ServerSpecV2{
			Addr: "localhost:3022",
		},
	}
	node.SetExpiry(time.Now().Add(2 * time.Second))
	clt, err := tt.server.NewClient(TestIdentity{
		I: BuiltinRole{
			Role:     types.RoleNode,
			Username: fmt.Sprintf("%v.%v", node.Metadata.Name, tt.server.ClusterName()),
		},
	})
	require.NoError(t, err)
	defer clt.Close()

	keepAlive, err := clt.UpsertNode(ctx, node)
	require.NoError(t, err)
	require.NotNil(t, keepAlive)

	keepAliver, err := clt.NewKeepAliver(ctx)
	require.NoError(t, err)
	defer keepAliver.Close()

	keepAlive.Expires = time.Now().Add(2 * time.Second)
	select {
	case keepAliver.KeepAlives() <- *keepAlive:
		// ok
	case <-time.After(time.Second):
		t.Fatalf("time out sending keep ailve")
	case <-keepAliver.Done():
		t.Fatalf("unknown problem sending keep ailve")
	}

	// upsert node and keep alives will fail for users with no privileges
	nopClt, err := tt.server.NewClient(TestBuiltin(types.RoleNop))
	require.NoError(t, err)
	defer nopClt.Close()

	_, err = nopClt.UpsertNode(ctx, node)
	require.True(t, trace.IsAccessDenied(err))

	k2, err := nopClt.NewKeepAliver(ctx)
	require.NoError(t, err)

	keepAlive.Expires = time.Now().Add(2 * time.Second)
	go func() {
		select {
		case k2.KeepAlives() <- *keepAlive:
		case <-k2.Done():
		}
	}()

	select {
	case <-time.After(time.Second):
		t.Fatalf("time out expecting error")
	case <-k2.Done():
	}

	require.True(t, trace.IsAccessDenied(k2.Error()))
}

// TestEventsPermissions tests events with regards
// to certificate authority rotation
func TestEventsPermissions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestBuiltin(types.RoleNode))
	require.NoError(t, err)
	defer clt.Close()

	w, err := clt.NewWatcher(ctx, types.Watch{Kinds: []types.WatchKind{{Kind: types.KindCertAuthority}}})
	require.NoError(t, err)
	defer w.Close()

	select {
	case <-time.After(2 * time.Second):
		t.Fatalf("Timeout waiting for init event")
	case event := <-w.Events():
		require.Equal(t, event.Type, types.OpInit)
	}

	// start rotation
	gracePeriod := time.Hour
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseInit,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	ca, err := tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, false)
	require.NoError(t, err)

	suite.ExpectResource(t, w, 3*time.Second, ca)

	type testCase struct {
		name     string
		identity TestIdentity
		watches  []types.WatchKind
	}

	testCases := []testCase{
		{
			name:     "node role is not authorized to get certificate authority with secret data loaded",
			identity: TestBuiltin(types.RoleNode),
			watches:  []types.WatchKind{{Kind: types.KindCertAuthority, LoadSecrets: true}},
		},
		{
			name:     "node role is not authorized to watch static tokens",
			identity: TestBuiltin(types.RoleNode),
			watches:  []types.WatchKind{{Kind: types.KindStaticTokens}},
		},
		{
			name:     "node role is not authorized to watch provisioning tokens",
			identity: TestBuiltin(types.RoleNode),
			watches:  []types.WatchKind{{Kind: types.KindToken}},
		},
		{
			name:     "nop role is not authorized to watch users and roles",
			identity: TestBuiltin(types.RoleNop),
			watches: []types.WatchKind{
				{Kind: types.KindUser},
				{Kind: types.KindRole},
			},
		},
		{
			name:     "nop role is not authorized to watch cert authorities",
			identity: TestBuiltin(types.RoleNop),
			watches:  []types.WatchKind{{Kind: types.KindCertAuthority, LoadSecrets: false}},
		},
		{
			name:     "nop role is not authorized to watch cluster config resources",
			identity: TestBuiltin(types.RoleNop),
			watches: []types.WatchKind{
				{Kind: types.KindClusterAuthPreference},
				{Kind: types.KindClusterNetworkingConfig},
				{Kind: types.KindSessionRecordingConfig},
			},
		},
	}

	tryWatch := func(tc testCase) {
		client, err := tt.server.NewClient(tc.identity)
		require.NoError(t, err)
		defer client.Close()

		watcher, err := client.NewWatcher(ctx, types.Watch{
			Kinds: tc.watches,
		})
		require.NoError(t, err)
		defer watcher.Close()

		go func() {
			select {
			case <-watcher.Events():
			case <-watcher.Done():
			}
		}()

		select {
		case <-time.After(time.Second):
			t.Fatalf("time out expecting error in test %q", tc.name)
		case <-watcher.Done():
		}

		require.True(t, trace.IsAccessDenied(watcher.Error()))
	}

	for _, tc := range testCases {
		tryWatch(tc)
	}
}

// TestEvents tests events suite
func TestEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	suite := &suite.ServicesTestSuite{
		ConfigS:       clt,
		EventsS:       clt,
		PresenceS:     clt,
		CAS:           clt,
		ProvisioningS: clt,
		Access:        clt,
		UsersS:        clt,
	}
	suite.Events(t)
}

// TestEventsClusterConfig test cluster configuration
func TestEventsClusterConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestBuiltin(types.RoleAdmin))
	require.NoError(t, err)
	defer clt.Close()

	w, err := clt.NewWatcher(ctx, types.Watch{Kinds: []types.WatchKind{
		{Kind: types.KindCertAuthority, LoadSecrets: true},
		{Kind: types.KindStaticTokens},
		{Kind: types.KindToken},
		{Kind: types.KindClusterAuditConfig},
		{Kind: types.KindClusterName},
	}})
	require.NoError(t, err)
	defer w.Close()

	select {
	case <-time.After(2 * time.Second):
		t.Fatalf("Timeout waiting for init event")
	case event := <-w.Events():
		require.Equal(t, event.Type, types.OpInit)
	}

	// start rotation
	gracePeriod := time.Hour
	err = tt.server.Auth().RotateCertAuthority(ctx, RotateRequest{
		Type:        types.HostCA,
		GracePeriod: &gracePeriod,
		TargetPhase: types.RotationPhaseInit,
		Mode:        types.RotationModeManual,
	})
	require.NoError(t, err)

	ca, err := tt.server.Auth().GetCertAuthority(ctx, types.CertAuthID{
		DomainName: tt.server.ClusterName(),
		Type:       types.HostCA,
	}, true)
	require.NoError(t, err)

	suite.ExpectResource(t, w, 3*time.Second, ca)

	// set static tokens
	staticTokens, err := types.NewStaticTokens(types.StaticTokensSpecV2{
		StaticTokens: []types.ProvisionTokenV1{
			{
				Token:   "tok1",
				Roles:   types.SystemRoles{types.RoleNode},
				Expires: time.Now().UTC().Add(time.Hour),
			},
		},
	})
	require.NoError(t, err)

	err = tt.server.Auth().SetStaticTokens(staticTokens)
	require.NoError(t, err)

	staticTokens, err = tt.server.Auth().GetStaticTokens()
	require.NoError(t, err)
	suite.ExpectResource(t, w, 3*time.Second, staticTokens)

	// create provision token and expect the update event
	token, err := types.NewProvisionToken(
		"tok2", types.SystemRoles{types.RoleProxy}, time.Now().UTC().Add(3*time.Hour))
	require.NoError(t, err)

	err = tt.server.Auth().UpsertToken(ctx, token)
	require.NoError(t, err)

	token, err = tt.server.Auth().GetToken(ctx, token.GetName())
	require.NoError(t, err)

	suite.ExpectResource(t, w, 3*time.Second, token)

	// delete token and expect delete event
	err = tt.server.Auth().DeleteToken(ctx, token.GetName())
	require.NoError(t, err)
	suite.ExpectDeleteResource(t, w, 3*time.Second, &types.ResourceHeader{
		Kind:    types.KindToken,
		Version: types.V2,
		Metadata: types.Metadata{
			Namespace: apidefaults.Namespace,
			Name:      token.GetName(),
		},
	})

	// update audit config
	auditConfig, err := types.NewClusterAuditConfig(types.ClusterAuditConfigSpecV2{
		AuditEventsURI: []string{"dynamodb://audit_table_name", "file:///home/log"},
	})
	require.NoError(t, err)
	err = tt.server.Auth().SetClusterAuditConfig(ctx, auditConfig)
	require.NoError(t, err)

	auditConfigResource, err := tt.server.Auth().GetClusterAuditConfig(ctx)
	require.NoError(t, err)
	suite.ExpectResource(t, w, 3*time.Second, auditConfigResource)

	// update cluster name resource metadata
	clusterNameResource, err := tt.server.Auth().GetClusterName()
	require.NoError(t, err)

	// update the resource with different labels to test the change
	clusterName := &types.ClusterNameV2{
		Kind:    types.KindClusterName,
		Version: types.V2,
		Metadata: types.Metadata{
			Name:      types.MetaNameClusterName,
			Namespace: apidefaults.Namespace,
			Labels: map[string]string{
				"key": "val",
			},
		},
		Spec: clusterNameResource.(*types.ClusterNameV2).Spec,
	}

	err = tt.server.Auth().DeleteClusterName()
	require.NoError(t, err)
	err = tt.server.Auth().SetClusterName(clusterName)
	require.NoError(t, err)

	clusterNameResource, err = tt.server.Auth().GetClusterName()
	require.NoError(t, err)
	suite.ExpectResource(t, w, 3*time.Second, clusterNameResource)
}

func TestNetworkRestrictions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tt := setupAuthContext(ctx, t)

	clt, err := tt.server.NewClient(TestAdmin())
	require.NoError(t, err)

	suite := &suite.ServicesTestSuite{
		RestrictionsS: clt,
	}
	suite.NetworkRestrictions(t)
}

// verifyJWT verifies that the token was signed by one the passed in key pair.
func verifyJWT(clock clockwork.Clock, clusterName string, pairs []*types.JWTKeyPair, token string) (*jwt.Claims, error) {
	errs := []error{}
	for _, pair := range pairs {
		publicKey, err := utils.ParsePublicKey(pair.PublicKey)
		if err != nil {
			errs = append(errs, trace.Wrap(err))
			continue
		}

		key, err := jwt.New(&jwt.Config{
			Clock:       clock,
			PublicKey:   publicKey,
			Algorithm:   defaults.ApplicationTokenAlgorithm,
			ClusterName: clusterName,
		})
		if err != nil {
			errs = append(errs, trace.Wrap(err))
			continue
		}
		claims, err := key.Verify(jwt.VerifyParams{
			RawToken: token,
			Username: "foo",
			URI:      "http://localhost:8080",
		})
		if err != nil {
			errs = append(errs, trace.Wrap(err))
			continue
		}
		return claims, nil
	}
	return nil, trace.NewAggregate(errs...)
}

func newTestTLSServer(t *testing.T) *TestTLSServer {
	as, err := NewTestAuthServer(TestAuthServerConfig{
		Dir:   t.TempDir(),
		Clock: clockwork.NewFakeClock(),
	})
	require.NoError(t, err)

	srv, err := as.NewTestTLSServer()
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	return srv
}
