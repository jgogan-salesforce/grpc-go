/*
 *
 * Copyright 2023 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package xdsclient

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/testutils/xds/e2e"
	"google.golang.org/grpc/xds/internal"
	"google.golang.org/grpc/xds/internal/testutils"
	xdstestutils "google.golang.org/grpc/xds/internal/testutils"
	"google.golang.org/grpc/xds/internal/xdsclient/bootstrap"
	"google.golang.org/grpc/xds/internal/xdsclient/xdsresource"
	"google.golang.org/grpc/xds/internal/xdsclient/xdsresource/version"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3listenerpb "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	v3discoverypb "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"

	_ "google.golang.org/grpc/xds/internal/httpfilter/router" // Register the router filter.
)

var emptyServerOpts = e2e.ManagementServerOptions{}

var (
	// Listener resource type implementation retrieved from the resource type map
	// in the internal package, which is initialized when the individual resource
	// types are created.
	listenerResourceType = internal.ResourceTypeMapForTesting[version.V3ListenerURL].(xdsresource.Type)
	rtRegistry           = newResourceTypeRegistry()
)

func init() {
	// Simulating maybeRegister for listenerResourceType. The getter to this registry
	// is passed to the authority for accessing the resource type.
	rtRegistry.types[listenerResourceType.TypeURL()] = listenerResourceType
}

func setupTest(ctx context.Context, t *testing.T, opts e2e.ManagementServerOptions, watchExpiryTimeout time.Duration) (*authority, *e2e.ManagementServer, string) {
	t.Helper()
	nodeID := uuid.New().String()
	ms, err := e2e.StartManagementServer(opts)
	if err != nil {
		t.Fatalf("Failed to spin up the xDS management server: %q", err)
	}

	a, err := newAuthority(authorityArgs{
		serverCfg: xdstestutils.ServerConfigForAddress(t, ms.Address),
		bootstrapCfg: &bootstrap.Config{
			NodeProto: &v3corepb.Node{Id: nodeID},
		},
		serializer:         grpcsync.NewCallbackSerializer(ctx),
		resourceTypeGetter: rtRegistry.get,
		watchExpiryTimeout: watchExpiryTimeout,
		logger:             nil,
	})
	if err != nil {
		t.Fatalf("Failed to create authority: %q", err)
	}
	return a, ms, nodeID
}

// This tests verifies watch and timer state for the scenario where a watch for
// an LDS resource is registered and the management server sends an update the
// same resource.
func (s) TestTimerAndWatchStateOnSendCallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	// Setting up a mgmt server with a done channel when OnStreamRequest is invoked.
	serverOnReqDoneCh := make(chan struct{})
	serverOpt := e2e.ManagementServerOptions{
		OnStreamRequest: func(int64, *v3discoverypb.DiscoveryRequest) error {
			select {
			case serverOnReqDoneCh <- struct{}{}:
			default:
			}
			return nil
		},
	}
	a, ms, nodeID := setupTest(ctx, t, serverOpt, defaultTestTimeout)
	defer ms.Stop()
	defer a.close()

	rn := "xdsclient-test-lds-resource"
	w := testutils.NewTestResourceWatcher()
	cancelResource := a.watchResource(listenerResourceType, rn, w)
	defer cancelResource()

	if err := compareWatchState(a, rn, watchStateStarted); err != nil {
		t.Fatal(err)
	}

	// This blocking read is to verify that the underlying transport has successfully
	// sent the request to the server, hence the onSend callback was already invoked.
	// onSend callback should transition the watchState to `watchStateRequested`.
	<-serverOnReqDoneCh
	if err := compareWatchState(a, rn, watchStateRequested); err != nil {
		t.Fatal(err)
	}

	// Updating mgmt server with the same lds resource. Blocking on watcher's update
	// ch to verify the watch state transition to `watchStateReceived`.
	if err := updateResourceInServer(ctx, ms, rn, nodeID); err != nil {
		t.Fatalf("Failed to update server with resource: %q; err: %q", rn, err)
	}
	for {
		select {
		case <-ctx.Done():
			t.Fatal("Test timed out before watcher received an update from server.")
		case <-w.ErrorCh:
		case <-w.UpdateCh:
			// This means the OnUpdate callback was invoked and the watcher was notified.
			if err := compareWatchState(a, rn, watchStateReceived); err != nil {
				t.Fatal(err)
			}
			return
		}
	}

}

// This tests the resource's watch state transition when the ADS stream is closed
// by the management server. After the test calls `watchResource` api to register
// a watch for a resource, it stops the management server, and verifies the resource's
// watch state transitions to `watchStateStarted` and timer ready to be restarted.
func (s) TestTimerAndWatchStateOnErrorCallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	a, ms, _ := setupTest(ctx, t, emptyServerOpts, defaultTestTimeout)
	defer a.close()

	rn := "xdsclient-test-lds-resource"
	w := testutils.NewTestResourceWatcher()
	cancelResource := a.watchResource(listenerResourceType, rn, w)
	defer cancelResource()

	// Stopping the server and blocking on watcher's err channel to be notified.
	// This means the onErr callback should be invoked which transitions the watch
	// state to `watchStateStarted`.
	ms.Stop()

	select {
	case <-ctx.Done():
		t.Fatal("Test timed out before verifying error propagation.")
	case err := <-w.ErrorCh:
		if xdsresource.ErrType(err) != xdsresource.ErrorTypeConnection {
			t.Fatal("Connection error not propagated to watchers.")
		}
	}

	if err := compareWatchState(a, rn, watchStateStarted); err != nil {
		t.Fatal(err)
	}
}

// This tests the case where the ADS stream breaks after successfully receiving
// a message on the stream. The test performs the following:
//   - configures the management server with resourceA.
//   - registers a watch for resourceA and verifies that the watcher's update
//     callback is invoked.
//   - registers a watch for resourceB and verifies that the watcher's update
//     callback is not invoked. This is because the management server does not
//     contain resourceB.
//   - stops the management server to verify that the error propagated to the
//     watcher is a connection error. This happens when the authority attempts
//     to create a new stream.
func (s) TestWatchResourceTimerCanRestartOnIgnoredADSRecvError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Using a shorter expiry timeout to verify that the watch timeout was never fired.
	a, ms, nodeID := setupTest(ctx, t, emptyServerOpts, defaultTestWatchExpiryTimeout)
	defer a.close()

	nameA := "xdsclient-test-lds-resourceA"
	watcherA := testutils.NewTestResourceWatcher()
	cancelA := a.watchResource(listenerResourceType, nameA, watcherA)

	if err := updateResourceInServer(ctx, ms, nameA, nodeID); err != nil {
		t.Fatalf("Failed to update server with resource: %q; err: %q", nameA, err)
	}

	// Blocking on resource A watcher's update Channel to verify that there is
	// more than one msg(s) received the ADS stream.
	select {
	case <-ctx.Done():
		t.Fatal("Test timed out before watcher received the update.")
	case err := <-watcherA.ErrorCh:
		t.Fatalf("Watch got an unexpected error update: %q; want: valid update.", err)
	case <-watcherA.UpdateCh:
	}

	nameB := "xdsclient-test-lds-resourceB"
	watcherB := testutils.NewTestResourceWatcher()
	cancelB := a.watchResource(listenerResourceType, nameB, watcherB)
	defer cancelB()

	// Blocking on resource B watcher's error channel. This error should be due to
	// connectivity issue when reconnecting because the mgmt server was already been
	// stopped. ALl other errors or an update will fail the test.
	cancelA()
	ms.Stop()
	select {
	case <-ctx.Done():
		t.Fatal("Test timed out before mgmt server got the request.")
	case u := <-watcherB.UpdateCh:
		t.Fatalf("Watch got an unexpected resource update: %v.", u)
	case gotErr := <-watcherB.ErrorCh:
		wantErr := xdsresource.ErrorTypeConnection
		if xdsresource.ErrType(gotErr) != wantErr {
			t.Fatalf("Watch got an unexpected error:%q. Want: %q.", gotErr, wantErr)
		}
	}

	// Since there was already a response on the stream, the timer for resource B
	// should not fire. If the timer did fire, watch state would be in `watchStateTimeout`.
	<-time.After(defaultTestWatchExpiryTimeout)
	if err := compareWatchState(a, nameB, watchStateStarted); err != nil {
		t.Fatalf("Invalid watch state: %v.", err)
	}

}

func compareWatchState(a *authority, rn string, wantState watchState) error {
	a.resourcesMu.Lock()
	defer a.resourcesMu.Unlock()
	gotState := a.resources[listenerResourceType][rn].wState
	if gotState != wantState {
		return fmt.Errorf("Got %v. Want: %v", gotState, wantState)
	}

	wTimer := a.resources[listenerResourceType][rn].wTimer
	switch gotState {
	case watchStateRequested:
		if wTimer == nil {
			return fmt.Errorf("got nil timer, want active timer")
		}
	case watchStateStarted:
		if wTimer != nil {
			return fmt.Errorf("got active timer, want nil timer")
		}
	default:
		if wTimer.Stop() {
			// This means that the timer was running but could be successfully stopped.
			return fmt.Errorf("got active timer, want stopped timer")
		}
	}
	return nil
}

func updateResourceInServer(ctx context.Context, ms *e2e.ManagementServer, rn string, nID string) error {
	l := e2e.DefaultClientListener(rn, "new-rds-resource")
	resources := e2e.UpdateOptions{
		NodeID:         nID,
		Listeners:      []*v3listenerpb.Listener{l},
		SkipValidation: true,
	}
	return ms.Update(ctx, resources)
}
