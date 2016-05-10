package ovsMgr

import (
	"fmt"
	"l3/bgp/utils"
	"sync"

	"github.com/socketplane/libovsdb"
)

const (
	ROUTE_TABLE = "Route"
)

/*  Constructor for policy manager
 */
func NewOvsPolicyMgr(db *BGPOvsdbHandler) *OvsPolicyMgr {
	mgr := &OvsPolicyMgr{
		plugin: "ovsdb",
		dbmgr:  db,
	}
	mgr.redistributeLock = sync.RWMutex{}

	return mgr
}

func (mgr *OvsPolicyMgr) AddPolicy() {

}

func (mgr *OvsPolicyMgr) RemovePolicy() {

}

func (mgr *OvsPolicyMgr) Start() {
	mgr.ospf = make(chan bool)
	mgr.static = make(chan bool)
	mgr.connected = make(chan bool)
	go mgr.handleRedistribute()
}

func (mgr *OvsPolicyMgr) sendConnectedRoutes(add bool) {
	routeEntries, exists :=
		mgr.dbmgr.cache[ROUTE_TABLE]
	if !exists {
		return
	}
	for key, value := range routeEntries {
		if value.Fields["from"] == "connected" {
			utils.Logger.Info(fmt.Sprintln("Key:", key))
			utils.Logger.Info(fmt.Sprintln("Value:", value))
			nhId, ok := value.Fields["nexthops"].(libovsdb.UUID)
			if !ok {
				utils.Logger.Err(fmt.Sprintln("No next hop configured for",
					value.Fields["prefix"]))
				continue
			}
			utils.Logger.Info("nh uuid: " + nhId.GoUuid)
			nhs, exists := mgr.dbmgr.cache["Nexthop"]
			if len(nhs) < 1 {
				utils.Logger.Err(fmt.Sprintln("No next hop configured for",
					value.Fields["prefix"]))
				continue
			}
			utils.Logger.Info(fmt.Sprintln("nhs:", nhs))
			nh, exists := nhs[nhId.GoUuid]
			utils.Logger.Info(fmt.Sprintln("nh:", nh))
			if !exists {
				utils.Logger.Err(fmt.Sprintln("No next hop configured for",
					value.Fields["prefix"]))
				continue
			}
			portId, ok := nh.Fields["ports"].(libovsdb.UUID)
			if !ok {
				utils.Logger.Err(fmt.Sprintln("No port information for",
					value.Fields["prefix"]))
				continue
			}
			utils.Logger.Info(fmt.Sprintln("PortID information is", portId.GoUuid))
			ports, exists := mgr.dbmgr.cache["Port"]
			if len(ports) < 1 {
				utils.Logger.Err(fmt.Sprintln("No entry for", portId.GoUuid,
					"in Port Table"))
				continue
			}
			port, exists := ports[portId.GoUuid]
			if !exists {
				utils.Logger.Err(fmt.Sprintln("No entry for", portId.GoUuid,
					"in Port Table"))
				continue
			}
			ip := port.Fields["ip4_address"]
			utils.Logger.Info("Ip address for the port is " + ip.(string))
		}
	}
}

func (mgr *OvsPolicyMgr) handleRedistribute() {
	for {
		//@TODO: we need to set some local variable to be true indicating that
		// redistribute policy is set... so when a route is installed after bgp is configured
		// then we need to send that route to bgp server
		select {
		case conn := <-mgr.connected:
			mgr.sendConnectedRoutes(conn)
		case static := <-mgr.static:
			if static {
			}
		case ospf := <-mgr.ospf:
			if ospf {
			}
		}
	}
}