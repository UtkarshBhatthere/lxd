package cluster

import (
	"context"
	"sync"
	"time"

	log "gopkg.in/inconshreveable/log15.v2"

	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/endpoints"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
)

// eventHubMinHosts is the minimum number of members that must have the event-hub role to trigger switching into
// event-hub mode (where cluster members will only connect to event-hub members rather than all members when
// operating in the normal full-mesh mode).
const eventHubMinHosts = 2

// EventMode indicates the event distribution mode.
type EventMode string

// EventModeFullMesh is when every cluster member connects to every other cluster member to pull events.
const EventModeFullMesh EventMode = "full-mesh"

// EventModeHubServer is when the cluster is operating in event-hub mode and this server is designated as a hub
// server, meaning that it will only connect to the other event-hub members and not other members.
const EventModeHubServer EventMode = "hub-server"

// EventModeHubClient is when the cluster is operating in event-hub mode and this member is designated as a hub
// client, meaning that it is expected to connect to the event-hub members.
const EventModeHubClient EventMode = "hub-client"

var eventMode EventMode = EventModeFullMesh
var eventHubAddresses []string
var listeners = map[string]*lxd.EventListener{}
var listenersNotify = map[chan struct{}][]string{}
var listenersLock sync.Mutex
var listenersUpdateLock sync.Mutex

// ServerEventMode returns the event distribution mode that this local server is operating in.
func ServerEventMode() EventMode {
	listenersLock.Lock()
	defer listenersLock.Unlock()

	return eventMode
}

// RoleInSlice returns whether or not the rule is within the roles list.
func RoleInSlice(role db.ClusterRole, roles []db.ClusterRole) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}

	return false
}

// EventListenerWait waits for there to be listener connected to the specified address, or one of the event hubs
// if operating in event hub mode.
func EventListenerWait(ctx context.Context, address string) error {
	// Check if there is already a listener.
	listenersLock.Lock()
	listener, found := listeners[address]
	if found && listener.IsActive() {
		listenersLock.Unlock()
		return nil
	}

	listenAddresses := []string{address}

	// Check if operating in event hub mode and if one of the event hub connections is available.
	// If so then we are ready to receive events from all members.
	if eventMode != EventModeFullMesh {
		for _, eventHubAddress := range eventHubAddresses {
			listener, found := listeners[eventHubAddress]
			if found && listener.IsActive() {
				listenersLock.Unlock()
				return nil
			}

			listenAddresses = append(listenAddresses, eventHubAddress)
		}
	}

	// If not setup a notification for when the desired address or any of the event hubs connect.
	connected := make(chan struct{})
	listenersNotify[connected] = listenAddresses
	listenersLock.Unlock()

	defer func() {
		listenersLock.Lock()
		delete(listenersNotify, connected)
		listenersLock.Unlock()
	}()

	// Wait for the connected channel to be closed (indicating a new listener has been connected), and return.
	select {
	case <-connected:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// hubAddresses returns the addresses of members with event-hub role, and the event mode of the server.
// The event mode will only be hub-server or hub-client if at least eventHubMinHosts have an event-hub role.
// Otherwise the mode will be full-mesh.
func hubAddresses(localAddress string, members map[int64]APIHeartbeatMember) ([]string, EventMode) {
	var hubAddresses []string
	var localHasHubRole bool

	// Do a first pass of members to count the members with event-hub role, and whether we are a hub server.
	for _, member := range members {
		if RoleInSlice(db.ClusterRoleEventHub, member.Roles) {
			hubAddresses = append(hubAddresses, member.Address)

			if member.Address == localAddress {
				localHasHubRole = true
			}
		}
	}

	eventMode := EventModeFullMesh
	if len(hubAddresses) >= eventHubMinHosts {
		if localHasHubRole {
			eventMode = EventModeHubServer
		} else {
			eventMode = EventModeHubClient
		}
	}

	return hubAddresses, eventMode
}

// EventsUpdateListeners refreshes the cluster event listener connections.
func EventsUpdateListeners(endpoints *endpoints.Endpoints, cluster *db.Cluster, serverCert func() *shared.CertInfo, members map[int64]APIHeartbeatMember, f func(int64, api.Event)) {
	listenersUpdateLock.Lock()
	defer listenersUpdateLock.Unlock()

	// If no heartbeat members provided, populate from global database.
	if members == nil {
		var dbMembers []db.NodeInfo
		var offlineThreshold time.Duration

		err := cluster.Transaction(func(tx *db.ClusterTx) error {
			var err error

			dbMembers, err = tx.GetNodes()
			if err != nil {
				return err
			}

			offlineThreshold, err = tx.GetNodeOfflineThreshold()
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			logger.Warn("Failed to get current cluster members", log.Ctx{"err": err})
			return
		}

		members = make(map[int64]APIHeartbeatMember, len(dbMembers))
		for _, dbMember := range dbMembers {
			members[dbMember.ID] = APIHeartbeatMember{
				ID:            dbMember.ID,
				Name:          dbMember.Name,
				Address:       dbMember.Address,
				LastHeartbeat: dbMember.Heartbeat,
				Online:        !dbMember.IsOffline(offlineThreshold),
				Roles:         dbMember.Roles,
			}
		}
	}

	networkAddress := endpoints.NetworkAddress()
	hubAddresses, localEventMode := hubAddresses(networkAddress, members)

	// Store event hub addresses in global slice.
	listenersLock.Lock()
	eventHubAddresses = hubAddresses
	eventMode = localEventMode
	listenersLock.Unlock()

	keepListeners := make(map[string]struct{})
	wg := sync.WaitGroup{}
	for _, member := range members {
		// Don't bother trying to connect to ourselves or offline members.
		if member.Address == networkAddress || !member.Online {
			continue
		}

		if localEventMode != EventModeFullMesh && !RoleInSlice(db.ClusterRoleEventHub, member.Roles) {
			continue // Skip non-event-hub members if we are operating in event-hub mode.
		}

		listenersLock.Lock()
		listener, ok := listeners[member.Address]

		// If the member already has a listener associated to it, check that the listener is still active.
		// If it is, just move on to next member, but if not then we'll try to connect again.
		if ok {
			if listener.IsActive() {
				keepListeners[member.Address] = struct{}{} // Add to current listeners list.
				listenersLock.Unlock()
				continue
			}

			// Disconnect and delete listener, but don't delete any listenersNotify entry as there
			// might be something waiting for a future connection.
			listener.Disconnect()
			delete(listeners, member.Address)
			logger.Info("Removed inactive member event listener client", log.Ctx{"local": networkAddress, "remote": member.Address})
		}
		listenersLock.Unlock()

		keepListeners[member.Address] = struct{}{} // Add to current listeners list.

		// Connect to remote concurrently and add to active listeners if successful.
		wg.Add(1)
		go func(m APIHeartbeatMember) {
			defer wg.Done()
			listener, err := eventsConnect(m.Address, endpoints.NetworkCert(), serverCert())
			if err != nil {
				logger.Warn("Failed adding member event listener client", log.Ctx{"local": networkAddress, "remote": m.Address, "err": err})
				return
			}

			listener.AddHandler(nil, func(event api.Event) { f(m.ID, event) })

			listenersLock.Lock()
			listeners[m.Address] = listener

			// Indicate to any notifiers waiting for this member's address that it is connected.
			for connected, notifyAddresses := range listenersNotify {
				if shared.StringInSlice(m.Address, notifyAddresses) {
					close(connected)
					delete(listenersNotify, connected)
				}
			}

			logger.Info("Added member event listener client", log.Ctx{"local": networkAddress, "remote": m.Address})
			listenersLock.Unlock()
		}(member)
	}

	wg.Wait()

	// Disconnect and delete any out of date listeners and their notifiers.
	listenersLock.Lock()
	for address, listener := range listeners {
		if _, found := keepListeners[address]; !found {
			listener.Disconnect()
			delete(listeners, address)
			logger.Info("Removed old member event listener client", log.Ctx{"local": networkAddress, "remote": address})
		}
	}
	listenersLock.Unlock()
}

// Establish a client connection to get events from the given node.
func eventsConnect(address string, networkCert *shared.CertInfo, serverCert *shared.CertInfo) (*lxd.EventListener, error) {
	client, err := Connect(address, networkCert, serverCert, nil, true)
	if err != nil {
		return nil, err
	}

	revert := revert.New()
	revert.Add(func() {
		client.Disconnect()
	})

	listener, err := client.GetEventsAllProjects()
	if err != nil {
		return nil, err
	}

	revert.Success()
	return listener, nil
}
