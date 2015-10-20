package shard

import (
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/jsonpb"
	"go.pachyderm.com/pachyderm/src/pkg/discovery"
	proto "go.pachyderm.com/pachyderm/src/pkg/shard/proto"
	log "go.pachyderm.com/pachyderm/src/pkg/shard/protolog"
	"go.pedge.io/protolog"
)

const InvalidVersion int64 = -1

var (
	holdTTL      uint64 = 20
	marshaler           = &jsonpb.Marshaler{}
	ErrCancelled        = fmt.Errorf("cancelled by user")
	errComplete         = fmt.Errorf("COMPLETE")
)

type sharder struct {
	discoveryClient discovery.Client
	numShards       uint64
	numReplicas     uint64
	namespace       string
	addresses       map[int64]*proto.Addresses
	addressesLock   sync.RWMutex
}

func newSharder(discoveryClient discovery.Client, numShards uint64, numReplicas uint64, namespace string) *sharder {
	return &sharder{discoveryClient, numShards, numReplicas, namespace, make(map[int64]*proto.Addresses), sync.RWMutex{}}
}

func (a *sharder) GetMasterAddress(shard uint64, version int64) (result string, ok bool, retErr error) {
	defer func() {
		protolog.Debug(&log.GetMasterAddress{shard, version, result, ok, errorToString(retErr)})
	}()
	addresses, err := a.getAddresses(version)
	if err != nil {
		return "", false, err
	}
	shardAddresses, ok := addresses.Addresses[shard]
	if !ok {
		return "", false, nil
	}
	return shardAddresses.Master, true, nil
}

func (a *sharder) GetReplicaAddresses(shard uint64, version int64) (result map[string]bool, retErr error) {
	defer func() {
		protolog.Debug(&log.GetReplicaAddresses{shard, version, result, errorToString(retErr)})
	}()
	addresses, err := a.getAddresses(version)
	if err != nil {
		return nil, err
	}
	shardAddresses, ok := addresses.Addresses[shard]
	if !ok {
		return nil, fmt.Errorf("shard %d not found", shard)
	}
	return shardAddresses.Replicas, nil
}

func (a *sharder) GetShardToMasterAddress(version int64) (result map[uint64]string, retErr error) {
	defer func() {
		protolog.Debug(&log.GetShardToMasterAddress{version, result, errorToString(retErr)})
	}()
	addresses, err := a.getAddresses(version)
	if err != nil {
		return nil, err
	}
	_result := make(map[uint64]string)
	for shard, shardAddresses := range addresses.Addresses {
		_result[shard] = shardAddresses.Master
	}
	return _result, nil
}

func (a *sharder) GetShardToReplicaAddresses(version int64) (result map[uint64]map[string]bool, retErr error) {
	defer func() {
		// We need resultPrime is because proto3 can't do maps of maps.
		resultPrime := make(map[uint64]*log.ReplicaAddresses)
		for shard, addresses := range result {
			resultPrime[shard] = &log.ReplicaAddresses{addresses}
		}
		protolog.Debug(&log.GetShardToReplicaAddresses{version, resultPrime, errorToString(retErr)})
	}()
	addresses, err := a.getAddresses(version)
	if err != nil {
		return nil, err
	}
	_result := make(map[uint64]map[string]bool)
	for shard, shardAddresses := range addresses.Addresses {
		_result[shard] = shardAddresses.Replicas
	}
	return _result, nil
}

func (a *sharder) Register(cancel chan bool, id string, address string, server Server) (retErr error) {
	protolog.Info(&log.StartRegister{id, address})
	defer func() {
		protolog.Info(&log.FinishRegister{id, address, errorToString(retErr)})
	}()
	var once sync.Once
	versionChan := make(chan int64)
	internalCancel := make(chan bool)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if err := a.announceServer(id, address, server, versionChan, internalCancel); err != nil {
			once.Do(func() {
				retErr = err
				close(internalCancel)
			})
		}
	}()
	go func() {
		defer wg.Done()
		if err := a.fillRoles(id, server, versionChan, internalCancel); err != nil {
			once.Do(func() {
				retErr = err
				close(internalCancel)
			})
		}
	}()
	go func() {
		defer wg.Done()
		select {
		case <-cancel:
			once.Do(func() {
				retErr = ErrCancelled
				close(internalCancel)
			})
		case <-internalCancel:
		}
	}()
	wg.Wait()
	return
}

func (a *sharder) RegisterFrontend(cancel chan bool, address string, frontend Frontend) (retErr error) {
	var once sync.Once
	versionChan := make(chan int64)
	internalCancel := make(chan bool)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if err := a.announceFrontend(address, frontend, versionChan, internalCancel); err != nil {
			once.Do(func() {
				retErr = err
				close(internalCancel)
			})
		}
	}()
	go func() {
		defer wg.Done()
		if err := a.runFrontend(address, frontend, versionChan, internalCancel); err != nil {
			once.Do(func() {
				retErr = err
				close(internalCancel)
			})
		}
	}()
	go func() {
		defer wg.Done()
		select {
		case <-cancel:
			once.Do(func() {
				retErr = ErrCancelled
				close(internalCancel)
			})
		case <-internalCancel:
		}
	}()
	wg.Wait()
	return
}

func (a *sharder) AssignRoles(cancel chan bool) (retErr error) {
	protolog.Info(&log.StartAssignRoles{})
	defer func() {
		protolog.Info(&log.FinishAssignRoles{errorToString(retErr)})
	}()
	var version int64
	oldServers := make(map[string]bool)
	oldRoles := make(map[string]*proto.ServerRole)
	oldMasters := make(map[uint64]string)
	oldReplicas := make(map[uint64][]string)
	var oldMinVersion int64
	// Reconstruct state from a previous run
	serverRoles, err := a.discoveryClient.GetAll(a.serverRoleDir())
	if err != nil {
		return err
	}
	for _, encodedServerRole := range serverRoles {
		serverRole, err := decodeServerRole(encodedServerRole)
		if err != nil {
			return err
		}
		if oldServerRole, ok := oldRoles[serverRole.Id]; !ok || oldServerRole.Version < serverRole.Version {
			oldRoles[serverRole.Id] = serverRole
			oldServers[serverRole.Id] = true
		}
		if version < serverRole.Version+1 {
			version = serverRole.Version + 1
		}
	}
	for _, oldServerRole := range oldRoles {
		for shard := range oldServerRole.Masters {
			oldMasters[shard] = oldServerRole.Id
		}
		for shard := range oldServerRole.Replicas {
			oldReplicas[shard] = append(oldReplicas[shard], oldServerRole.Id)
		}
	}
	err = a.discoveryClient.WatchAll(a.serverStateDir(), cancel,
		func(encodedServerStates map[string]string) error {
			if len(encodedServerStates) == 0 {
				return nil
			}
			newServerStates := make(map[string]*proto.ServerState)
			shardLocations := make(map[uint64][]string)
			newRoles := make(map[string]*proto.ServerRole)
			newMasters := make(map[uint64]string)
			newReplicas := make(map[uint64][]string)
			masterRolesPerServer := a.numShards / uint64(len(encodedServerStates))
			masterRolesRemainder := a.numShards % uint64(len(encodedServerStates))
			replicaRolesPerServer := (a.numShards * a.numReplicas) / uint64(len(encodedServerStates))
			replicaRolesRemainder := (a.numShards * a.numReplicas) % uint64(len(encodedServerStates))
			for _, encodedServerState := range encodedServerStates {
				serverState, err := decodeServerState(encodedServerState)
				if err != nil {
					return err
				}
				newServerStates[serverState.Id] = serverState
				newRoles[serverState.Id] = &proto.ServerRole{
					Id:       serverState.Id,
					Version:  version,
					Masters:  make(map[uint64]bool),
					Replicas: make(map[uint64]bool),
				}
				for shard := range serverState.Shards {
					shardLocations[shard] = append(shardLocations[shard], serverState.Id)
				}
			}
			// See if there's any roles we can delete
			minVersion := int64(math.MaxInt64)
			for _, serverState := range newServerStates {
				if serverState.Version < minVersion {
					minVersion = serverState.Version
				}
			}
			// Delete roles that no servers are using anymore
			if minVersion > oldMinVersion {
				oldMinVersion = minVersion
				if err := a.discoveryClient.WatchAll(
					a.frontendStateDir(),
					cancel,
					func(encodedFrontendStates map[string]string) error {
						for _, encodedFrontendState := range encodedFrontendStates {
							frontendState, err := decodeFrontendState(encodedFrontendState)
							if err != nil {
								return err
							}
							if frontendState.Version < minVersion {
								return nil
							}
						}
						return errComplete
					}); err != nil && err != errComplete {
					return err
				}
				serverRoles, err := a.discoveryClient.GetAll(a.serverRoleDir())
				if err != nil {
					return err
				}
				for key, encodedServerRole := range serverRoles {
					serverRole, err := decodeServerRole(encodedServerRole)
					if err != nil {
						return err
					}
					if serverRole.Version < minVersion {
						if err := a.discoveryClient.Delete(key); err != nil {
							return err
						}
						protolog.Info(&log.DeleteServerRole{serverRole})
					}
				}
			}
			// if the servers are identical to last time then we know we'll
			// assign shards the same way
			if sameServers(oldServers, newServerStates) {
				return nil
			}
		Master:
			for shard := uint64(0); shard < a.numShards; shard++ {
				if id, ok := oldMasters[shard]; ok {
					if assignMaster(newRoles, newMasters, id, shard, masterRolesPerServer, &masterRolesRemainder) {
						continue Master
					}
				}
				for _, id := range oldReplicas[shard] {
					if assignMaster(newRoles, newMasters, id, shard, masterRolesPerServer, &masterRolesRemainder) {
						continue Master
					}
				}
				for _, id := range shardLocations[shard] {
					if assignMaster(newRoles, newMasters, id, shard, masterRolesPerServer, &masterRolesRemainder) {
						continue Master
					}
				}
				for id := range newServerStates {
					if assignMaster(newRoles, newMasters, id, shard, masterRolesPerServer, &masterRolesRemainder) {
						continue Master
					}
				}
				protolog.Error(&log.FailedToAssignRoles{
					ServerStates: newServerStates,
					NumShards:    a.numShards,
					NumReplicas:  a.numReplicas,
				})
				return nil
			}
			for replica := uint64(0); replica < a.numReplicas; replica++ {
			Replica:
				for shard := uint64(0); shard < a.numShards; shard++ {
					if id, ok := oldMasters[shard]; ok {
						if assignReplica(newRoles, newMasters, newReplicas, id, shard, replicaRolesPerServer, &replicaRolesRemainder) {
							continue Replica
						}
					}
					for _, id := range oldReplicas[shard] {
						if assignReplica(newRoles, newMasters, newReplicas, id, shard, replicaRolesPerServer, &replicaRolesRemainder) {
							continue Replica
						}
					}
					for _, id := range shardLocations[shard] {
						if assignReplica(newRoles, newMasters, newReplicas, id, shard, replicaRolesPerServer, &replicaRolesRemainder) {
							continue Replica
						}
					}
					for id := range newServerStates {
						if assignReplica(newRoles, newMasters, newReplicas, id, shard, replicaRolesPerServer, &replicaRolesRemainder) {
							continue Replica
						}
					}
					for id := range newServerStates {
						if swapReplica(newRoles, newMasters, newReplicas, id, shard, replicaRolesPerServer) {
							continue Replica
						}
					}
					protolog.Error(&log.FailedToAssignRoles{
						ServerStates: newServerStates,
						NumShards:    a.numShards,
						NumReplicas:  a.numReplicas,
					})
					return nil
				}
			}
			addresses := proto.Addresses{
				Version:   version,
				Addresses: make(map[uint64]*proto.ShardAddresses),
			}
			for shard := uint64(0); shard < a.numShards; shard++ {
				addresses.Addresses[shard] = &proto.ShardAddresses{Replicas: make(map[string]bool)}
			}
			for id, serverRole := range newRoles {
				encodedServerRole, err := marshaler.MarshalToString(serverRole)
				if err != nil {
					return err
				}
				if err := a.discoveryClient.Set(a.serverRoleKeyVersion(id, version), encodedServerRole, 0); err != nil {
					return err
				}
				protolog.Info(&log.SetServerRole{serverRole})
				address := newServerStates[id].Address
				for shard := range serverRole.Masters {
					shardAddresses := addresses.Addresses[shard]
					shardAddresses.Master = address
					addresses.Addresses[shard] = shardAddresses
				}
				for shard := range serverRole.Replicas {
					shardAddresses := addresses.Addresses[shard]
					shardAddresses.Replicas[address] = true
					addresses.Addresses[shard] = shardAddresses
				}
			}
			encodedAddresses, err := marshaler.MarshalToString(&addresses)
			if err != nil {
				return err
			}
			if err := a.discoveryClient.Set(a.addressesKey(version), encodedAddresses, 0); err != nil {
				return err
			}
			protolog.Info(&log.SetAddresses{&addresses})
			version++
			oldServers = make(map[string]bool)
			for id := range newServerStates {
				oldServers[id] = true
			}
			oldRoles = newRoles
			oldMasters = newMasters
			oldReplicas = newReplicas
			return nil
		})
	if err == discovery.ErrCancelled {
		return ErrCancelled
	}
	return err
}

func (a *sharder) WaitForAvailability(frontendIds []string, serverIds []string) error {
	version := InvalidVersion
	if err := a.discoveryClient.WatchAll(a.serverDir(), nil,
		func(encodedServerStatesAndRoles map[string]string) error {
			serverStates := make(map[string]*proto.ServerState)
			serverRoles := make(map[string]map[int64]*proto.ServerRole)
			for key, encodedServerStateOrRole := range encodedServerStatesAndRoles {
				if strings.HasPrefix(key, a.serverStateDir()) {
					serverState, err := decodeServerState(encodedServerStateOrRole)
					if err != nil {
						return err
					}
					serverStates[serverState.Id] = serverState
				}
				if strings.HasPrefix(key, a.serverRoleDir()) {
					serverRole, err := decodeServerRole(encodedServerStateOrRole)
					if err != nil {
						return err
					}
					if _, ok := serverRoles[serverRole.Id]; !ok {
						serverRoles[serverRole.Id] = make(map[int64]*proto.ServerRole)
					}
					serverRoles[serverRole.Id][serverRole.Version] = serverRole
				}
			}
			if len(serverStates) != len(serverIds) {
				return nil
			}
			if len(serverRoles) != len(serverIds) {
				return nil
			}
			for _, id := range serverIds {
				if _, ok := serverStates[id]; !ok {
					return nil
				}
				if _, ok := serverRoles[id]; !ok {
					return nil
				}
			}
			versions := make(map[int64]bool)
			for _, serverState := range serverStates {
				if serverState.Version == InvalidVersion {
					return nil
				}
				versions[serverState.Version] = true
			}
			if len(versions) != 1 {
				return nil
			}
			for _, versionToServerRole := range serverRoles {
				if len(versionToServerRole) != 1 {
					return nil
				}
				for version := range versionToServerRole {
					if !versions[version] {
						return nil
					}
				}
			}
			// This loop actually does something, it sets the outside
			// version variable.
			for version = range versions {
			}
			return errComplete
		}); err != errComplete {
		return err
	}

	if err := a.discoveryClient.WatchAll(
		a.frontendStateDir(),
		nil,
		func(encodedFrontendStates map[string]string) error {
			frontendStates := make(map[string]*proto.FrontendState)
			for _, encodedFrontendState := range encodedFrontendStates {
				frontendState, err := decodeFrontendState(encodedFrontendState)
				if err != nil {
					return err
				}

				if frontendState.Version != version {
					protolog.Printf("Wrong version: %d != %d", frontendState.Version, version)
					return nil
				}
				frontendStates[frontendState.Address] = frontendState
			}
			protolog.Printf("frontendStates: %+v", frontendStates)
			if len(frontendStates) != len(frontendIds) {
				return nil
			}
			for _, id := range frontendIds {
				if _, ok := frontendStates[id]; !ok {
					return nil
				}
			}
			return errComplete
		}); err != nil && err != errComplete {
		return err
	}
	return nil
}

func (a *sharder) routeDir() string {
	return fmt.Sprintf("%s/pfs/route", a.namespace)
}

func (a *sharder) serverDir() string {
	return path.Join(a.routeDir(), "server")
}

func (a *sharder) serverStateDir() string {
	return path.Join(a.serverDir(), "state")
}

func (a *sharder) serverStateKey(address string) string {
	return path.Join(a.serverStateDir(), address)
}

func (a *sharder) serverRoleDir() string {
	return path.Join(a.serverDir(), "role")
}

func (a *sharder) serverRoleKey(address string) string {
	return path.Join(a.serverRoleDir(), address)
}

func (a *sharder) serverRoleKeyVersion(address string, version int64) string {
	return path.Join(a.serverRoleKey(address), fmt.Sprint(version))
}

func (a *sharder) frontendDir() string {
	return path.Join(a.routeDir(), "frontend")
}

func (a *sharder) frontendStateDir() string {
	return path.Join(a.frontendDir(), "state")
}

func (a *sharder) frontendStateKey(address string) string {
	return path.Join(a.frontendStateDir(), address)
}

func (a *sharder) addressesDir() string {
	return path.Join(a.routeDir(), "addresses")
}

func (a *sharder) addressesKey(version int64) string {
	return path.Join(a.addressesDir(), fmt.Sprint(version))
}

func decodeServerState(encodedServerState string) (*proto.ServerState, error) {
	var serverState proto.ServerState
	if err := jsonpb.UnmarshalString(encodedServerState, &serverState); err != nil {
		return nil, err
	}
	return &serverState, nil
}

func decodeFrontendState(encodedFrontendState string) (*proto.FrontendState, error) {
	var frontendState proto.FrontendState
	if err := jsonpb.UnmarshalString(encodedFrontendState, &frontendState); err != nil {
		return nil, err
	}
	return &frontendState, nil
}

func (a *sharder) getServerStates() (map[string]*proto.ServerState, error) {
	encodedServerStates, err := a.discoveryClient.GetAll(a.serverStateDir())
	if err != nil {
		return nil, err
	}
	result := make(map[string]*proto.ServerState)
	for _, encodedServerState := range encodedServerStates {
		serverState, err := decodeServerState(encodedServerState)
		if err != nil {
			return nil, err
		}
		result[serverState.Id] = serverState
	}
	return result, nil
}

func (a *sharder) getServerState(id string) (*proto.ServerState, error) {
	encodedServerState, err := a.discoveryClient.Get(a.serverStateKey(id))
	if err != nil {
		return nil, err
	}
	return decodeServerState(encodedServerState)
}

func decodeServerRole(encodedServerRole string) (*proto.ServerRole, error) {
	var serverRole proto.ServerRole
	if err := jsonpb.UnmarshalString(encodedServerRole, &serverRole); err != nil {
		return nil, err
	}
	return &serverRole, nil
}

func (a *sharder) getServerRoles() (map[string]map[int64]*proto.ServerRole, error) {
	encodedServerRoles, err := a.discoveryClient.GetAll(a.serverRoleDir())
	if err != nil {
		return nil, err
	}
	result := make(map[string]map[int64]*proto.ServerRole)
	for _, encodedServerRole := range encodedServerRoles {
		serverRole, err := decodeServerRole(encodedServerRole)
		if err != nil {
			return nil, err
		}
		if _, ok := result[serverRole.Id]; !ok {
			result[serverRole.Id] = make(map[int64]*proto.ServerRole)
		}
		result[serverRole.Id][serverRole.Version] = serverRole
	}
	return result, nil
}

func (a *sharder) getServerRole(id string) (map[int64]*proto.ServerRole, error) {
	encodedServerRoles, err := a.discoveryClient.GetAll(a.serverRoleKey(id))
	if err != nil {
		return nil, err
	}
	result := make(map[int64]*proto.ServerRole)
	for _, encodedServerRole := range encodedServerRoles {
		serverRole, err := decodeServerRole(encodedServerRole)
		if err != nil {
			return nil, err
		}
		result[serverRole.Version] = serverRole
	}
	return result, nil
}

func (a *sharder) getAddresses(version int64) (*proto.Addresses, error) {
	if version == InvalidVersion {
		return nil, fmt.Errorf("invalid version")
	}
	a.addressesLock.RLock()
	if addresses, ok := a.addresses[version]; ok {
		a.addressesLock.RUnlock()
		return addresses, nil
	}
	a.addressesLock.RUnlock()
	a.addressesLock.Lock()
	defer a.addressesLock.Unlock()
	encodedAddresses, err := a.discoveryClient.Get(a.addressesKey(version))
	if err != nil {
		return nil, err
	}
	var addresses proto.Addresses
	if err := jsonpb.UnmarshalString(encodedAddresses, &addresses); err != nil {
		return nil, err
	}
	a.addresses[version] = &addresses
	return &addresses, nil
}

func hasShard(serverRole *proto.ServerRole, shard uint64) bool {
	return serverRole.Masters[shard] || serverRole.Replicas[shard]
}

func removeReplica(replicas map[uint64][]string, shard uint64, id string) {
	var ids []string
	for _, replicaID := range replicas[shard] {
		if id != replicaID {
			ids = append(ids, replicaID)
		}
	}
	replicas[shard] = ids
}

func assignMaster(
	serverRoles map[string]*proto.ServerRole,
	masters map[uint64]string,
	id string,
	shard uint64,
	masterRolesPerServer uint64,
	masterRolesRemainder *uint64,
) bool {
	serverRole, ok := serverRoles[id]
	if !ok {
		return false
	}
	if uint64(len(serverRole.Masters)) > masterRolesPerServer {
		return false
	}
	if uint64(len(serverRole.Masters)) == masterRolesPerServer && *masterRolesRemainder == 0 {
		return false
	}
	if hasShard(serverRole, shard) {
		return false
	}
	if uint64(len(serverRole.Masters)) == masterRolesPerServer && *masterRolesRemainder > 0 {
		*masterRolesRemainder--
	}
	serverRole.Masters[shard] = true
	serverRoles[id] = serverRole
	masters[shard] = id
	return true
}

func assignReplica(
	serverRoles map[string]*proto.ServerRole,
	masters map[uint64]string,
	replicas map[uint64][]string,
	id string,
	shard uint64,
	replicaRolesPerServer uint64,
	replicaRolesRemainder *uint64,
) bool {
	serverRole, ok := serverRoles[id]
	if !ok {
		return false
	}
	if uint64(len(serverRole.Replicas)) > replicaRolesPerServer {
		return false
	}
	if uint64(len(serverRole.Replicas)) == replicaRolesPerServer && *replicaRolesRemainder == 0 {
		return false
	}
	if hasShard(serverRole, shard) {
		return false
	}
	if uint64(len(serverRole.Replicas)) == replicaRolesPerServer && *replicaRolesRemainder > 0 {
		*replicaRolesRemainder--
	}
	serverRole.Replicas[shard] = true
	serverRoles[id] = serverRole
	replicas[shard] = append(replicas[shard], id)
	return true
}

func swapReplica(
	serverRoles map[string]*proto.ServerRole,
	masters map[uint64]string,
	replicas map[uint64][]string,
	id string,
	shard uint64,
	replicaRolesPerServer uint64,
) bool {
	serverRole, ok := serverRoles[id]
	if !ok {
		return false
	}
	if uint64(len(serverRole.Replicas)) >= replicaRolesPerServer {
		return false
	}
	for swapID, swapServerRole := range serverRoles {
		if swapID == id {
			continue
		}
		for swapShard := range swapServerRole.Replicas {
			if hasShard(serverRole, swapShard) {
				continue
			}
			if hasShard(swapServerRole, shard) {
				continue
			}
			delete(swapServerRole.Replicas, swapShard)
			serverRoles[swapID] = swapServerRole
			removeReplica(replicas, swapShard, swapID)
			// We do some weird things with the limits here, both servers
			// receive a 0 replicaRolesRemainder, swapID doesn't need a
			// remainder because we're replacing a shard we stole so it also
			// has MaxInt64 for replicaRolesPerServer. We already know id
			// doesn't need the remainder since we check that it has fewer than
			// replicaRolesPerServer replicas.
			var noReplicaRemainder uint64
			assignReplica(serverRoles, masters, replicas, swapID, shard, math.MaxUint64, &noReplicaRemainder)
			assignReplica(serverRoles, masters, replicas, id, swapShard, replicaRolesPerServer, &noReplicaRemainder)
			return true
		}
	}
	return false
}

func (a *sharder) announceServer(
	id string,
	address string,
	server Server,
	versionChan chan int64,
	cancel chan bool,
) error {
	serverState := &proto.ServerState{
		Id:      id,
		Address: address,
		Version: InvalidVersion,
	}
	for {
		shards, err := server.LocalShards()
		if err != nil {
			return err
		}
		serverState.Shards = shards
		encodedServerState, err := marshaler.MarshalToString(serverState)
		if err != nil {
			return err
		}
		if err := a.discoveryClient.Set(a.serverStateKey(id), encodedServerState, holdTTL); err != nil {
			return err
		}
		protolog.Debug(&log.SetServerState{serverState})
		select {
		case <-cancel:
			return nil
		case version := <-versionChan:
			serverState.Version = version
		case <-time.After(time.Second * time.Duration(holdTTL/2)):
		}
	}
}

func (a *sharder) announceFrontend(
	address string,
	frontend Frontend,
	versionChan chan int64,
	cancel chan bool,
) error {
	frontendState := &proto.FrontendState{
		Address: address,
		Version: InvalidVersion,
	}
	for {
		encodedFrontendState, err := marshaler.MarshalToString(frontendState)
		if err != nil {
			return err
		}
		if err := a.discoveryClient.Set(a.frontendStateKey(address), encodedFrontendState, holdTTL); err != nil {
			return err
		}
		protolog.Debug(&log.SetFrontendState{frontendState})
		select {
		case <-cancel:
			return nil
		case version := <-versionChan:
			protolog.Printf("Got frontend version: %d", version)
			frontendState.Version = version
		case <-time.After(time.Second * time.Duration(holdTTL/2)):
		}
	}
}

type int64Slice []int64

func (s int64Slice) Len() int           { return len(s) }
func (s int64Slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s int64Slice) Less(i, j int) bool { return s[i] < s[j] }

func (a *sharder) fillRoles(
	id string,
	server Server,
	versionChan chan int64,
	cancel chan bool,
) error {
	oldRoles := make(map[int64]proto.ServerRole)
	return a.discoveryClient.WatchAll(
		a.serverRoleKey(id),
		cancel,
		func(encodedServerRoles map[string]string) error {
			roles := make(map[int64]proto.ServerRole)
			var versions int64Slice
			// Decode the roles
			for _, encodedServerRole := range encodedServerRoles {
				var serverRole proto.ServerRole
				if err := jsonpb.UnmarshalString(encodedServerRole, &serverRole); err != nil {
					return err
				}
				roles[serverRole.Version] = serverRole
				versions = append(versions, serverRole.Version)
			}
			sort.Sort(versions)
			if len(versions) > 2 {
				versions = versions[0:2]
			}
			// For each new version bring the server up to date
			for _, version := range versions {
				if _, ok := oldRoles[version]; ok {
					// we've already seen these roles, so nothing to do here
					continue
				}
				serverRole := roles[version]
				var wg sync.WaitGroup
				var addShardErr error
				for _, shard := range shards(serverRole) {
					if !containsShard(oldRoles, shard) {
						wg.Add(1)
						shard := shard
						go func() {
							defer wg.Done()
							if err := server.AddShard(shard, version-1); err != nil && addShardErr == nil {
								addShardErr = err
							}
						}()
					}
				}
				wg.Wait()
				if addShardErr != nil {
					protolog.Info(&log.AddServerRole{&serverRole, addShardErr.Error()})
					return addShardErr
				}
				protolog.Info(&log.AddServerRole{&serverRole, ""})
				oldRoles[version] = serverRole
				versionChan <- version
			}
			// See if there are any old roles that aren't needed
			for version, serverRole := range oldRoles {
				var wg sync.WaitGroup
				var removeShardErr error
				if _, ok := roles[version]; ok {
					// these roles haven't expired yet, so nothing to do
					continue
				}
				for _, shard := range shards(serverRole) {
					if !containsShard(roles, shard) {
						wg.Add(1)
						shard := shard
						go func(shard uint64) {
							defer wg.Done()
							if err := server.RemoveShard(shard, version-1); err != nil && removeShardErr == nil {
								removeShardErr = err
							}
						}(shard)
					}
				}
				wg.Wait()
				if removeShardErr != nil {
					protolog.Info(&log.RemoveServerRole{&serverRole, removeShardErr.Error()})
					return removeShardErr
				}
				protolog.Info(&log.RemoveServerRole{&serverRole, ""})
			}
			oldRoles = make(map[int64]proto.ServerRole)
			for _, version := range versions {
				oldRoles[version] = roles[version]
			}
			return nil
		},
	)
}

func (a *sharder) runFrontend(
	id string,
	frontend Frontend,
	versionChan chan int64,
	cancel chan bool,
) error {
	version := InvalidVersion
	return a.discoveryClient.WatchAll(
		a.serverStateDir(),
		cancel,
		func(encodedServerStates map[string]string) error {
			if len(encodedServerStates) == 0 {
				return nil
			}
			minVersion := int64(math.MaxInt64)
			for _, encodedServerState := range encodedServerStates {
				serverState, err := decodeServerState(encodedServerState)
				if err != nil {
					return err
				}
				if serverState.Version < minVersion {
					minVersion = serverState.Version
				}
				protolog.Printf("serverState: %+v", serverState)
			}
			if minVersion > version {
				if err := frontend.Version(minVersion); err != nil {
					return err
				}
				version = minVersion
				versionChan <- version
			}
			return nil
		})
}

func shards(serverRole proto.ServerRole) []uint64 {
	var result []uint64
	for shard := range serverRole.Masters {
		result = append(result, shard)
	}
	for shard := range serverRole.Replicas {
		result = append(result, shard)
	}
	return result
}

func containsShard(roles map[int64]proto.ServerRole, shard uint64) bool {
	for _, serverRole := range roles {
		if serverRole.Masters[shard] || serverRole.Replicas[shard] {
			return true
		}
	}
	return false
}

func sameServers(oldServers map[string]bool, newServerStates map[string]*proto.ServerState) bool {
	if len(oldServers) != len(newServerStates) {
		return false
	}
	for id := range oldServers {
		if _, ok := newServerStates[id]; !ok {
			return false
		}
	}
	return true
}

// TODO this code is duplicate elsewhere, we should put it somehwere.
func errorToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}