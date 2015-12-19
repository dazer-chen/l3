package main

import (
        "os"
        "syscall"
	"arpd"
	"asicdServices"
	"bytes"
        "reflect"
        "strings"
	"encoding/json"
	"fmt"
	"git.apache.org/thrift.git/lib/go/thrift"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
        "github.com/vishvananda/netlink"
	"github.com/google/gopacket/pcap"
        _ "github.com/mattn/go-sqlite3"
	"io/ioutil"
	"log/syslog"
	"net"
	"portdServices"
	"strconv"
	"time"
        "os/signal"
        "database/sql"
        "utils/dbutils"
)

const (
	ARP_ERR_NOT_FOUND = iota
	ARP_PARSE_ADDR_FAIL
	ARP_ERR_REQ_FAIL
	ARP_ERR_RESP_FAIL
	ARP_ERR_ADD_FAIL
	ARP_REQ_SUCCESS
	ARP_ERR_LAST
)

const (
	ARP_ADD_ENTRY = iota
	ARP_DEL_ENTRY
	ARP_UPDATE_ENTRY
)

type ARPClientBase struct {
	Address            string
	Transport          thrift.TTransport
	PtrProtocolFactory *thrift.TBinaryProtocolFactory
	IsConnected        bool
}

type AsicdClient struct {
	ARPClientBase
	ClientHdl *asicdServices.AsicdServiceClient
}

type PortdClient struct {
	ARPClientBase
	ClientHdl *portdServices.PortServiceClient
}

type arpEntry struct {
	macAddr net.HardwareAddr
	vlanid  arpd.Int
        valid   bool
        counter int
        port    int
        ifName  string
        ifType  arpd.Int
        localIP string
}

type arpCache struct {
	cacheTimeout time.Duration
	arpMap       map[string]arpEntry
	//dev_handle   *pcap.Handle
	hostTO       time.Duration
	routerTO     time.Duration
}

type ClientJson struct {
	Name string `json:Name`
	Port int    `json:Port`
}
type PortConfigJson struct {
	Port   int    `json:Port`
	Ifname string `json:Ifname`
}

type arpUpdateMsg struct {
        ip string
        ent arpEntry
        msg_type int
}

type pcapHandle struct {
        pcap_handle     *pcap.Handle
        ifName          string
}

type portProperty struct {
    untagged_vlanid     arpd.Int
}

/*
 * connection params.
 */
var (
	//device          string = "fpPort2"
	//device       string = "eth0"
	snapshot_len int32  = 65549 //packet capture length
	//promiscuous  bool   = true //mode
	promiscuous  bool   = false //mode
	err          error
	timeout_pcap      time.Duration = 10 * time.Second
	timeout      time.Duration = 60 * time.Second
        timeout_counter int = 10
        retry_cnt    int    = 2
	handle       *pcap.Handle  // handle for pcap connection
	//device_ip    string        = "40.1.1.1"
	//device_ip       string = "10.0.2.15"
	//filter_string   string = "arp host 10.1.10.1"
	//filter_optimize int    = 0
	logWriter       *syslog.Writer
	log_err         error
	//rec_handle      []*pcap.Handle
        dbHdl           *sql.DB
        UsrConfDbName   string = "/../bin/UsrConfDb.db"
        dump_arp_table  bool = false
)
var arp_cache *arpCache
var asicdClient AsicdClient //Thrift client to connect to asicd
var portdClient PortdClient //portd services client

var pcap_handle_map map[int]pcapHandle
var port_property_map map[int]portProperty

//var portCfgList []PortConfigJson

var arp_cache_update_chl chan arpUpdateMsg = make(chan arpUpdateMsg, 100)


/*** TEMP DEFINES **/
//var myMac = "00:11:22:33:44:55"
//var myMac = "08:00:27:75:bc:4d"
//var myMac = "fa:15:f5:69:a4:c9"

/****** Utility APIs.****/
func getIP(ipAddr string) (ip net.IP, err int) {
	ip = net.ParseIP(ipAddr)
	if ip == nil {
		return ip, ARP_PARSE_ADDR_FAIL
	}
	ip = ip.To4()
	return ip, ARP_REQ_SUCCESS
}

func getHWAddr(macAddr string) (mac net.HardwareAddr, err error) {
	mac, err = net.ParseMAC(macAddr)
	if mac == nil {
		return mac, err
	}

	return mac, nil
}

func getMacAddrInterfaceName(ifName string) (macAddr string, err error) {

        ifi, err := net.InterfaceByName(ifName)
        if err != nil {
            logWriter.Err(fmt.Sprintf("Failed to get the mac address of ", ifName))
            return macAddr, err
        }
        macAddr = ifi.HardwareAddr.String()
	return macAddr, nil
}

func getIPv4ForInterfaceName(ifname string) (iface_ip string, err error) {
    interfaces, err := net.Interfaces()
    if err != nil {
        logWriter.Err(fmt.Sprintf("Failed to get the interface"))
        return "", err
    }
    for _, inter := range interfaces {
        if inter.Name == ifname {
            if addrs, err := inter.Addrs(); err == nil {
                for _, addr := range addrs {
                    switch ip := addr.(type) {
                        case *net.IPNet:
                            if ip.IP.DefaultMask() != nil {
                                return (ip.IP).String(), nil
                            }
                    }
                }
            } else {
                logWriter.Err(fmt.Sprintf("Failed to get the ip address of", ifname))
                return "", err
            }
        }
    }
    return "", err
}

func getIPv4ForInterface(iftype arpd.Int, vlan_id arpd.Int) (ip_addr string, err error) {
    var if_name string

    if iftype == 0 { //VLAN
        if_name = fmt.Sprintf("SVI%d", vlan_id)
    } else if iftype == 1 { //PHY
        if_name = fmt.Sprintf("fpPort-", vlan_id)
    } else {
        return "", err
    }

    logger.Println("Local Interface name =", if_name)
    return getIPv4ForInterfaceName(if_name)
}

//Note: Caller validates that portStr is a valid port range string
func parsePortRange(portStr string) (int, int, error) {
        portNums := strings.Split(portStr, "-")
        startPort, err := strconv.Atoi(portNums[0])
        if err != nil {
                return 0, 0, err
        }
        endPort, err := strconv.Atoi(portNums[1])
        if err != nil {
                return 0, 0, err
        }
        return startPort, endPort, nil
}


/*
 * Utility function to parse from a user specified port string to a port bitmap.
 * Supported formats for port string shown below:
 * - 1,2,3,10 (comma separate list of ports)
 * - 1-10,24,30-31 (hypen separated port ranges)
 * - 00011 (direct port bitmap)
 */
func parseUsrPortStrToPbm(usrPortStr string) (string, error) {
        //FIXME: Assuming max of 256 ports, create common def (another instance in main.go)
        var portList [256]int
        var pbmStr string = ""
        //Handle ',' separated strings
        if strings.Contains(usrPortStr, ",") {
                commaSepList := strings.Split(usrPortStr, ",")
                for _, subStr := range commaSepList {
                        //Substr contains '-' separated range
                        if strings.Contains(subStr, "-") {
                                startPort, endPort, err := parsePortRange(subStr)
                                if err != nil {
                                        return pbmStr, err
                                }
                                for port := startPort; port <= endPort; port++ {
                                        portList[port] = 1
                                }
                        } else {
                                //Substr is a port number
                                port, err := strconv.Atoi(subStr)
                                if err != nil {
                                        return pbmStr, err
                                }
                                portList[port] = 1
                        }
                }
        } else if strings.Contains(usrPortStr, "-") {
                //Handle '-' separated range
                startPort, endPort, err := parsePortRange(usrPortStr)
                if err != nil {
                        return pbmStr, err
                }
                for port := startPort; port <= endPort; port++ {
                        portList[port] = 1
                }
        } else {
        if len(usrPortStr) > 1 {
            //Port bitmap directly specified
            return usrPortStr, nil
        } else {
            //Handle single port number
            port, err := strconv.Atoi(usrPortStr)
            if err != nil {
                return pbmStr, err
            }
            portList[port] = 1
        }
        }
        //Convert portList to port bitmap string
        var zeroStr string = ""
        for _, port := range portList {
                if port == 1 {
                        pbmStr += zeroStr
                        pbmStr += "1"
                        zeroStr = ""
                } else {
                        zeroStr += "0"
                }
        }
        return pbmStr, nil
}

/***** Thrift APIs ******/
func (m ARPServiceHandler) UpdateUntaggedPortToVlanMap(vlanid arpd.Int,
        untaggedPorts string) (rval arpd.Int, err error) {

    logger.Println("Received UpdateUntaggedPortToVlanMap(): vlanid:", vlanid, "ports:", untaggedPorts)

    portTagStr, err := parseUsrPortStrToPbm(untaggedPorts)
    if err != nil {
        return 0, err
    }

    for i := 0; i < len(portTagStr); i++ {
        if (portTagStr[i] - '0') == 1 {
            ent := port_property_map[i]
            ent.untagged_vlanid = vlanid
            port_property_map[i] = ent
        }
    }

    return rval, nil
}

func (m ARPServiceHandler) ResolveArpIPV4(targetIp string,
	iftype arpd.Int, vlan_id arpd.Int) (rc arpd.Int, err error) {

        logger.Println("Calling ResolveArpIPv4...", targetIp, " ", int32(iftype), " ", int32(vlan_id))
        ip_addr, err := getIPv4ForInterface(iftype, vlan_id)
        if len(ip_addr) == 0 || err != nil {
            logWriter.Err(fmt.Sprintf("Failed to get the ip address of ifType:", iftype, "VLAN:", vlan_id))
            return ARP_ERR_REQ_FAIL, err
        }
        logger.Println("Local IP address of is:", ip_addr)
        //var linux_device string
        if portdClient.IsConnected {
		linux_device, err := portdClient.ClientHdl.GetLinuxIfc(int32(iftype), int32(vlan_id))
/*
                for _, port_cfg := range portCfgList {
                    linux_device = port_cfg.Ifname
*/
                    logger.Println("linux_device ", linux_device)
                    if err != nil {
                            logWriter.Err(fmt.Sprintf("Failed to get ifname for interface : ", vlan_id, "type : ", iftype))
                            return ARP_ERR_REQ_FAIL, err
                    }
                    logWriter.Err(fmt.Sprintln("Server:Connecting to device ", linux_device))
                    handle, err = pcap.OpenLive(linux_device, snapshot_len, promiscuous, timeout_pcap)
                    if handle == nil {
                            logWriter.Err(fmt.Sprintln("Server: No device found.:device , err ", linux_device, err))
                            return 0, err
                    }
/*
                    mac_addr, err := getMacAddrInterfaceName(port_cfg.Ifname)
                    if err != nil {
                        logWriter.Err(fmt.Sprintln("Unable to get the MAC addr of ", port_cfg.Ifname))
                        continue
                    }
                    logger.Println("MAC addr of ", port_cfg.Ifname, ": ", mac_addr)
*/
                    mac_addr, err := getMacAddrInterfaceName(linux_device)
                    if err != nil {
                        logWriter.Err(fmt.Sprintln("Unable to get the MAC addr of ", linux_device))
                    }
                    logger.Println("MAC addr of ", linux_device, ": ", mac_addr)

                    go processPacket(targetIp, iftype, vlan_id, handle, mac_addr, ip_addr)
/*
                }
*/

	} else {
		logWriter.Err("portd client is not connected.")
		logger.Println("Portd is not connected.")
	}

	return ARP_REQ_SUCCESS, err

}

/*
 * @fn SetArpTimeout
 *     This API sets arp cache timeout.
 *     current defauls -
 *     hostTimeout = 10 sec
 *     routerTimeout = 10sec
 */
func (m ARPServiceHandler) SetArpTimeout(ifName string,
	hostTimeout int,
	routerTimeout int) (rc arpd.Int, err error) {
	cp := arp_cache
	if time.Duration(hostTimeout) > cp.hostTO {
		cp.hostTO = time.Duration(hostTimeout)
	}
	if time.Duration(routerTimeout) > cp.routerTO {
		cp.routerTO = time.Duration(routerTimeout)
	}
	return 0, nil

}

/*****Local API calls. *****/

/*
 * @fn ConnectToClients
 *     connect to other deamons such as asicd.
 */
func ConnectToClients(paramsFile string) {
	var clientsList []ClientJson

	bytes, err := ioutil.ReadFile(paramsFile)
	if err != nil {
		logWriter.Err("Error in reading configuration file")
		return
	}

	err = json.Unmarshal(bytes, &clientsList)
	if err != nil {
		logWriter.Err("Error in Unmarshalling Json")
		return
	}

	for _, client := range clientsList {
		logWriter.Err("#### Client name is ")
		logWriter.Err(client.Name)
		if client.Name == "asicd" {
			logger.Printf("found asicd at port %d", client.Port)
			asicdClient.Address = "localhost:" + strconv.Itoa(client.Port)
			asicdClient.Transport, asicdClient.PtrProtocolFactory = CreateIPCHandles(asicdClient.Address)
			if asicdClient.Transport != nil && asicdClient.PtrProtocolFactory != nil {
				logWriter.Info("connecting to asicd")
				asicdClient.ClientHdl = asicdServices.NewAsicdServiceClientFactory(asicdClient.Transport, asicdClient.PtrProtocolFactory)
				asicdClient.IsConnected = true
			}

		}
		if client.Name == "portd" {
			logger.Printf("found portd at port %d", client.Port)
			portdClient.Address = "localhost:" + strconv.Itoa(client.Port)
			portdClient.Transport, portdClient.PtrProtocolFactory = CreateIPCHandles(portdClient.Address)
			if portdClient.Transport != nil && portdClient.PtrProtocolFactory != nil {
				logWriter.Info("connecting to asicd")
				portdClient.ClientHdl = portdServices.NewPortServiceClientFactory(portdClient.Transport, portdClient.PtrProtocolFactory)
				portdClient.IsConnected = true
			}

		}
	}
}

func storeArpTableInDB(ifType int, vlanid int, ifName string, portid int, dest_ip string, src_ip string) error {
    var dbCmd string
    dbCmd = fmt.Sprintf(`INSERT INTO ARPCache (ifType, vlanid, ifName, portid, src_ip, key) VALUES ('%d', '%d', '%s', '%d', '%s', '%s') ;`, ifType, vlanid, ifName, portid, src_ip, dest_ip)
    logger.Println(dbCmd)
    if dbHdl != nil {
        logger.Println("Executing DB Command:", dbCmd)
        _, err = dbutils.ExecuteSQLStmt(dbCmd, dbHdl)
        if err != nil {
            logWriter.Err(fmt.Sprintln("Failed to Insert entry for", dest_ip, "in DB"))
            return err
        }
    } else {
        logger.Println("DB handler is nil");
    }
    return nil
}

func sigHandler(sigChan <-chan os.Signal) {
    signal := <-sigChan
    switch signal {
    case syscall.SIGHUP:
        //Cache the existing ARP entries
        logger.Println("Received SIGHUP signal")
        printArpEntries()
        logger.Println("Closing DB handler")
        if dbHdl != nil {
            dbHdl.Close()
        }
        os.Exit(0)
    default:
        logger.Println("Unhandled signal : ", signal)
    }
}

func intantiateDB() error {
    var err error
    err = nil
    DbName := params_dir + UsrConfDbName
    logger.Println("DB Location: ", DbName)
    dbHdl, err = sql.Open("sqlite3", DbName)
    if err != nil {
        logWriter.Err("Failed to create the handle")
        return err
    }

    if err = dbHdl.Ping(); err != nil {
        logWriter.Err("Failed to keep DB connection alive")
        return err
    }

    dbCmd := "CREATE TABLE IF NOT EXISTS ARPCache " +
            "(key string PRIMARY KEY ," +
            "ifType int, vlanid int, ifName string, portid int, src_ip string)"

    _, err = dbutils.ExecuteSQLStmt(dbCmd, dbHdl)
    if err != nil {
        logWriter.Err("Failed to create ARPCache Table in DB")
        return err
    }

    return err
}

func updateARPCacheFromDB() {
        var ent arpEntry
        var port_prop_ent portProperty
        var ip      string
        var ifType  int
        var vlanid  int
        var ifName  string
        var portid  int
        var src_ip  string
        //var dbCmd string

        logger.Println("Populate ARP Cache from DB entries")
        rows, err := dbHdl.Query("SELECT * FROM ARPCache")
        if err != nil {
            logWriter.Err(fmt.Sprintf("Unable to Query DB:", err))
            return
        }
        for rows.Next() {
            err = rows.Scan(&ip, &ifType, &vlanid, &ifName, &portid, &src_ip)
            if err != nil {
                logWriter.Err(fmt.Sprintf("Unable to Scan entry from DB:", err))
                return
            }
            logger.Println("Data Retrived From DB IP:", ip, "IFTYPE:", ifType, "VLANID:", vlanid, "IFNAME:", ifName, "PORTID:", portid, "SRC_IP:", src_ip)

            ent = arp_cache.arpMap[ip]
            ent.ifType = arpd.Int(ifType)
            ent.vlanid = arpd.Int(vlanid)
            ent.ifName = ifName
            ent.port = portid
            ent.localIP = src_ip
            ent.counter = 0
            ent.valid = true
            arp_cache.arpMap[ip] = ent
            port_prop_ent = port_property_map[portid]
            port_prop_ent.untagged_vlanid = arpd.Int(vlanid)
            port_property_map[portid] = port_prop_ent
        }

}

func refreshARPDB() {
        var dbCmd string
        dbCmd = "DELETE FROM ARPCache ;"
        logger.Println(dbCmd)
        if dbHdl != nil {
            logger.Println("Executing DB Command:", dbCmd)
            _, err = dbutils.ExecuteSQLStmt(dbCmd, dbHdl)
            if err != nil {
                logWriter.Err(fmt.Sprintln("Failed to Delete all ARP entries from DB"))
                return
            }
        } else {
            logger.Println("DB handler is nil");
        }
}

func initARPhandlerParams() {
	//init syslog
	logWriter, log_err = syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "ARPD_LOG")
	defer logWriter.Close()

	// Initialise arp cache.
	success := initArpCache()
        port_property_map = make(map[int]portProperty)
	if success != true {
		logWriter.Err("server: Failed to initialise ARP cache")
		logger.Println("Failed to initialise ARP cache")
		return
	}

        // init DB
        err := intantiateDB()
        if err != nil {
            logger.Println("DB intantiate failure: ", err)
        } else {
            logger.Println("ArpCache DB has been Initiated")
            updateARPCacheFromDB()
            refreshARPDB()
        }
	//connect to asicd and portd
	configFile := params_dir + "/clients.json"
	ConnectToClients(configFile)
        go updateArpCache()
        go timeout_thread()
        //List of signals to handle
        sigChan := make(chan os.Signal, 1)
        signalList := []os.Signal{syscall.SIGHUP}
        signal.Notify(sigChan, signalList...)
        go sigHandler(sigChan)
	initPortParams()
        /* Open Response thread */
        processResponse()

}

/*
func BuildAsicToLinuxMap(cfgFile string) {
	bytes, err := ioutil.ReadFile(cfgFile)
	if err != nil {
		logger.Println("Error in reading port configuration file")
		logWriter.Err(fmt.Sprintln("Error in reading port configuration file: ", err))
		return
	}
	err = json.Unmarshal(bytes, &portCfgList)
	if err != nil {
		logWriter.Err(fmt.Sprintln("Error in Unmarshalling Json, err=", err))
		return
	}
        pcap_handle_map = make(map[int]pcapHandle)
	for _, v := range portCfgList {
                logger.Println("BuildAsicToLinuxMap : iface = ", v.Ifname)
                logger.Println("BuildAsicToLinuxMap : port = ", v.Port)
		local_handle, err := pcap.OpenLive(v.Ifname, snapshot_len, promiscuous, timeout_pcap)
		if local_handle == nil {
			logWriter.Err(fmt.Sprintln("Server: No device found.: ", v.Ifname, err))
		}
                ent := pcap_handle_map[v.Port]
                ent.pcap_handle = local_handle
                ent.ifName = v.Ifname
                pcap_handle_map[v.Port] = ent
	}
}
*/
func BuildAsicToLinuxMap() {
        pcap_handle_map = make(map[int]pcapHandle)
        var ifName string
	for i := 1; i < 73; i++ {
                ifName = fmt.Sprintf("fpPort-%d", i)
                //logger.Println("BuildAsicToLinuxMap : iface = ", ifName)
                //logger.Println("BuildAsicToLinuxMap : port = ", i)
		local_handle, err := pcap.OpenLive(ifName, snapshot_len, promiscuous, timeout_pcap)
		if local_handle == nil {
			logWriter.Err(fmt.Sprintln("Server: No device found.: ", ifName, err))
		}
                ent := pcap_handle_map[i]
                ent.pcap_handle = local_handle
                ent.ifName = ifName
                pcap_handle_map[i] = ent
	}
}
func initPortParams() {
	//configFile := params_dir + "/clients.json"
	//ConnectToClients(configFile)
/*
	portCfgFile := params_dir + "/portd.json"
	BuildAsicToLinuxMap(portCfgFile)
*/
	BuildAsicToLinuxMap()
}

func processPacket(targetIp string, iftype arpd.Int, vlanid arpd.Int, handle *pcap.Handle, mac_addr string, localIp string) {
        logger.Println("processPacket() : Arp request for ", targetIp, "from", localIp)
/*
	_, exist := arp_cache.arpMap[targetIp]
	if !exist {
                sendArpReq(targetIp, handle, mac_addr, localIp)
                arp_cache_update_chl <- arpUpdateMsg {
                                            ip: targetIp,
                                            ent: arpEntry {
                                                    macAddr: []byte{0,0,0,0,0,0},
                                                    vlanid: vlanid,
                                                    valid: false,
                                                    port: -1,
                                                    ifName: "",
                                                    ifType: iftype,
                                                    localIP: localIp,
                                                    counter: timeout_counter,
                                                 },
                                            msg_type: 0,
                                         }
	} else {
            // get MAC from cache.
            logger.Println("ARP entry already existed")
            printArpEntries()
            return
        }
*/
	//_, exist := arp_cache.arpMap[targetIp]
	//if !exist {
        sendArpReq(targetIp, handle, mac_addr, localIp)
        arp_cache_update_chl <- arpUpdateMsg {
                                    ip: targetIp,
                                    ent: arpEntry {
                                            macAddr: []byte{0,0,0,0,0,0},
                                            vlanid: vlanid,
                                            valid: false,
                                            port: -1,
                                            ifName: "",
                                            ifType: iftype,
                                            localIP: localIp,
                                            counter: timeout_counter,
                                         },
                                    msg_type: 0,
                                 }
/*
	} else {
            // get MAC from cache.
            logger.Println("ARP entry already existed")
            printArpEntries()
            return
        }
*/

	// get MAC from cache.
        //logger.Println("ARP entry got created")
	//printArpEntries()
	return
}

func processResponse() {
        for port_id, p_hdl := range pcap_handle_map {
                //logger.Println("ifName = ", p_hdl.ifName, " Port = ", port_id)
                if p_hdl.pcap_handle == nil {
                    logger.Println("pcap handle is nil");
                    continue
                }
                mac_addr, err := getMacAddrInterfaceName(p_hdl.ifName)
                if err != nil {
                    logWriter.Err(fmt.Sprintln("Unable to get the MAC addr of ", p_hdl.ifName))
                    continue
                }
                //logger.Println("MAC addr of ", p_hdl.ifName, ": ", mac_addr)
                myMac_addr, fail := getHWAddr(mac_addr)
                if fail != nil {
                        logWriter.Err(fmt.Sprintf("corrupted my mac : ", mac_addr))
                        continue
                }
                go receiveArpResponse(p_hdl.pcap_handle, myMac_addr,
                                      port_id, p_hdl.ifName)
        }
	return
}

/*
 *@fn sendArpReq
 *  Send the ARP request for ip targetIP
 */
func sendArpReq(targetIp string, handle *pcap.Handle, myMac string, localIp string) int {
        logger.Println("sendArpReq(): sending arp requeust for targetIp ", targetIp,
                        "local IP ", localIp)

	source_ip, err := getIP(localIp)
	if err != ARP_REQ_SUCCESS {
		logWriter.Err(fmt.Sprintf("Corrupted source ip :  ", localIp))
		return ARP_ERR_REQ_FAIL
	}
	dest_ip, err := getIP(targetIp)
	if err != ARP_REQ_SUCCESS {
		logWriter.Err(fmt.Sprintf("Corrupted dest ip :  ", targetIp))
		return ARP_ERR_REQ_FAIL
	}
	myMac_addr, fail := getHWAddr(myMac)
	if fail != nil {
		logWriter.Err(fmt.Sprintf("corrupted my mac : ", myMac))
		return ARP_ERR_REQ_FAIL
	}
	arp_layer := layers.ARP{
		AddrType:          layers.LinkTypeEthernet,
		Protocol:          layers.EthernetTypeIPv4,
		HwAddressSize:     6,
		ProtAddressSize:   4,
		Operation:         layers.ARPRequest,
		SourceHwAddress:   myMac_addr,
		SourceProtAddress: source_ip,
		DstHwAddress:      []byte{0, 0, 0, 0, 0, 0},
	}
	eth_layer := layers.Ethernet{
		SrcMAC:       myMac_addr,
		DstMAC:       net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		EthernetType: layers.EthernetTypeARP,
	}

	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	arp_layer.DstProtAddress = dest_ip
	gopacket.SerializeLayers(buffer, options, &eth_layer, &arp_layer)

        //logger.Println("Buffer : ", buffer)
        // send arp request and retry after timeout if arp cache is not updated
        if err := handle.WritePacketData(buffer.Bytes()); err != nil {
            return ARP_ERR_REQ_FAIL
        }
	return ARP_REQ_SUCCESS
}

func getInterfaceNameByIndex(index int) (ifName string, err error) {
    ifi, err := net.InterfaceByIndex(index)
    if err != nil {
        logWriter.Err(fmt.Sprintf("Unable to get interface name.", ifi, err))
        return "", err
    }
    return ifi.Name, nil
}

/*
 *@fn receiveArpResponse
 * Process ARP response from the interface for ARP
 * req sent for targetIp
 */
func receiveArpResponse(rec_handle *pcap.Handle,
	myMac net.HardwareAddr, port_id int, if_Name string) {
        var src_Mac net.HardwareAddr

	src := gopacket.NewPacketSource(rec_handle, layers.LayerTypeEthernet)
	in := src.Packets()
	for {
		packet, ok := <-in
		if ok {
                        //logger.Println("Receive some packet on arp response thread")

			//vlan_layer := packet.Layer(layers.LayerTypeEthernet)
			//vlan_tag := vlan_layer.(*layers.Ethernet)
			//vlan_id := vlan_layer.LayerContents()
			//logWriter.Err(vlan_tag.)
			arpLayer := packet.Layer(layers.LayerTypeARP)
			if arpLayer != nil {
                            arp := arpLayer.(*layers.ARP)
                            if arp == nil {
                                    continue
                            }
                            if arp.Operation != layers.ARPReply || bytes.Equal([]byte(myMac), arp.SourceHwAddress) {
                                    continue
                            }


                            src_Mac = net.HardwareAddr(arp.SourceHwAddress)
                            src_ip_addr := (net.IP(arp.SourceProtAddress)).String()
                            dest_Mac := net.HardwareAddr(arp.DstHwAddress)
                            dest_ip_addr := (net.IP(arp.DstProtAddress)).String()
                            logger.Println("Received Arp response SRC_IP:", src_ip_addr, "SRC_MAC: ", src_Mac, "DST_IP:", dest_ip_addr, "DST_MAC:", dest_Mac)
                            ent, exist := arp_cache.arpMap[src_ip_addr]
                            if exist {
                                arp_cache_update_chl <- arpUpdateMsg {
                                                            ip: src_ip_addr,
                                                            ent: arpEntry {
                                                                    macAddr: src_Mac,
                                                                    vlanid: ent.vlanid,
                                                                    valid: true,
                                                                    port: port_id,
                                                                    ifName: if_Name,
                                                                    ifType: ent.ifType,
                                                                    localIP: ent.localIP,
                                                                    counter: timeout_counter,
                                                                 },
                                                            msg_type: 1,
                                                         }
                            } else {
                                port_map_ent, exists := port_property_map[port_id]
                                var vlan_id arpd.Int
                                if exists {
                                    vlan_id = port_map_ent.untagged_vlanid
                                } else {
                                    vlan_id = 1
                                }
                                arp_cache_update_chl <- arpUpdateMsg {
                                                            ip: src_ip_addr,
                                                            ent: arpEntry {
                                                                    macAddr: src_Mac,
                                                                    vlanid: vlan_id, // Need to be re-visited
                                                                    valid: true,
                                                                    port: port_id,
                                                                    ifName: if_Name,
                                                                    ifType: 1,
                                                                    localIP: dest_ip_addr,
                                                                    counter: timeout_counter,
                                                                 },
                                                            msg_type: 3,
                                                         }
                            }
			} else {
                            if nw := packet.NetworkLayer(); nw != nil {
                                src_ip, dst_ip := nw.NetworkFlow().Endpoints()
                                dst_ip_addr := dst_ip.String()
                                dstip := net.ParseIP(dst_ip_addr)
                                src_ip_addr := src_ip.String()
/*
                                if src_ip_addr == localIP || dst_ip_addr == localIP {
                                    continue
                                }
*/
                                _, exist := arp_cache.arpMap[dst_ip_addr]
                                if !exist {
                                    //dst_ip_addr := src_ip.String()
                                    route, err := netlink.RouteGet(dstip)
                                    var ifName string
                                    for _, rt := range route {
                                        if rt.LinkIndex > 1 {
                                            ifName, err = getInterfaceNameByIndex(rt.LinkIndex)
                                            if err != nil || ifName == "" {
                                                logWriter.Err(fmt.Sprintf("Unable to get the outgoing interface", err))
                                                continue
                                            }
                                        }
                                    }
                                    if ifName == "" {
                                        continue
                                    }
                                    logger.Println("Receive Some packet from src_ip:", src_ip_addr, "dst_ip:", dst_ip_addr, "Outgoing Interface:", ifName)
                                    go createAndSendArpReuqest(dst_ip_addr, ifName)
                                }
                            }

                        }
		}

	}
}

func createAndSendArpReuqest(targetIP string, outgoingIfName string) {
    localIp, err := getIPv4ForInterfaceName(outgoingIfName)
    if err != nil || localIp == "" {
        logWriter.Err(fmt.Sprintf("Unable to get the ip address of ", outgoingIfName))
        return
    }
    handle, err = pcap.OpenLive(outgoingIfName, snapshot_len, promiscuous, timeout_pcap)
    if handle == nil {
            logWriter.Err(fmt.Sprintln("Server: No device found.:device , err ", outgoingIfName, err))
            return
    }

    mac_addr, err := getMacAddrInterfaceName(outgoingIfName)
    if err != nil {
        logWriter.Err(fmt.Sprintln("Unable to get the MAC addr of ", outgoingIfName))
    }
    logger.Println("MAC addr of ", outgoingIfName, ": ", mac_addr)
    sendArpReq(targetIP, handle, mac_addr, localIp)
}

/*
 *@fn InitArpCache
 * Initiliase s/w cache. It also acts a reset API for timeout.
 */
func initArpCache() bool {
	arp_cache = &arpCache{arpMap: make(map[string]arpEntry)}
	//arp_cache.arpMap = make(map[string]arpEntry)
	logWriter.Err("InitArpCache done.")
	return true
}

/*
 * @fn UpdateArpCache
 *  Update IP to the ARP mapping for the hash table.
 */
func updateArpCache() {
    var cnt int
    var dbCmd string
        for {
            msg := <-arp_cache_update_chl
            if msg.msg_type == 1 {
            //if msg.ent.vlanid == 0 {
                ent := arp_cache.arpMap[msg.ip]
                if reflect.DeepEqual(ent.macAddr, msg.ent.macAddr) &&
                   ent.valid == msg.ent.valid && ent.port == msg.ent.port &&
                   ent.ifName == msg.ent.ifName && ent.vlanid == msg.ent.vlanid &&
                   ent.ifType == msg.ent.ifType {
                   logger.Println("Updating counter after retry after expiry")
                   ent.counter = msg.ent.counter
                   arp_cache.arpMap[msg.ip] = ent
                   continue
                }
                ent.macAddr = msg.ent.macAddr
                ent.valid = msg.ent.valid
                ent.vlanid = msg.ent.vlanid
                // Every entry will be expired after 10 mins
                ent.counter = msg.ent.counter
                ent.port    = msg.ent.port
                ent.ifName  = msg.ent.ifName
                ent.ifType  = msg.ent.ifType
                ent.localIP = msg.ent.localIP
                arp_cache.arpMap[msg.ip] = ent
                logger.Println("1 updateArpCache(): ", arp_cache.arpMap[msg.ip])
                err := storeArpTableInDB(int(ent.ifType), int(ent.vlanid), ent.ifName, int(ent.port), msg.ip, ent.localIP)
                if err != nil {
                    logWriter.Err("Unable to cache ARP Table in DB")
                }
                //3) Update asicd.
                if asicdClient.IsConnected {
/*
                        logger.Println("1. Deleting an entry in asic for ", msg.ip)
                        rv, error := asicdClient.ClientHdl.DeleteIPv4Neighbor(msg.ip,
                                             "00:00:00:00:00:00", 0, 0)
                        logWriter.Err(fmt.Sprintf("Asicd Del rv: ", rv, " error : ", error))
*/
                        logger.Println("1. Updating an entry in asic for ", msg.ip)
                        rv, error := asicdClient.ClientHdl.UpdateIPv4Neighbor(msg.ip,
                                             (msg.ent.macAddr).String(), (int32)(arp_cache.arpMap[msg.ip].vlanid), (int32)(msg.ent.port))
                        logWriter.Err(fmt.Sprintf("Asicd Update rv: ", rv, " error : ", error))
                } else {
                        logWriter.Err("1. Asicd client is not connected.")
                }
            } else if msg.msg_type == 0 {
                ent := arp_cache.arpMap[msg.ip]
                ent.vlanid = msg.ent.vlanid
                ent.valid = msg.ent.valid
                ent.counter = msg.ent.counter
                ent.port    = msg.ent.port
                ent.ifName  = msg.ent.ifName
                ent.ifType  = msg.ent.ifType
                ent.localIP = msg.ent.localIP
                arp_cache.arpMap[msg.ip] = ent
            } else if msg.msg_type == 2 {
                for ip, arp := range arp_cache.arpMap {
                    if arp.counter == -2 && arp.valid == true {
                        dbCmd = fmt.Sprintf(`DELETE FROM ARPCache WHERE key='%s' ;`, ip)
                        logger.Println(dbCmd)
                        if dbHdl != nil {
                            logger.Println("Executing DB Command:", dbCmd)
                            _, err = dbutils.ExecuteSQLStmt(dbCmd, dbHdl)
                            if err != nil {
                                logWriter.Err(fmt.Sprintln("Failed to Delete ARP entries from DB for %s %s", ip, err))
                            }
                        } else {
                            logger.Println("DB handler is nil");
                        }
                        logger.Println("1. Deleting entry ", ip, " from Arp cache")
                        delete(arp_cache.arpMap, ip)
                        logger.Println("Deleting an entry in asic for ", ip)
                        rv, error := asicdClient.ClientHdl.DeleteIPv4Neighbor(ip,
                                             "00:00:00:00:00:00", 0, 0)
                        logWriter.Err(fmt.Sprintf("Asicd Del rv: ", rv, " error : ", error))
                    } else if (arp.counter == 0 || arp.counter == -1) && arp.valid == true {
                        ent := arp_cache.arpMap[ip]
                        cnt = arp.counter
                        cnt--
                        ent.counter = cnt
                        //logger.Println("1. Decrementing counter for ", ip);
                        arp_cache.arpMap[ip] = ent
                        //Send arp request after entry expires
                        refresh_arp_entry(ip, ent.ifName, ent.localIP)
                    } else if ((arp.counter <= (timeout_counter)) &&
                               (arp.counter > (timeout_counter - retry_cnt))) &&
                              arp.valid == false {
                        ent := arp_cache.arpMap[ip]
                        cnt = arp.counter
                        cnt--
                        ent.counter = cnt
                        //logger.Println("2. Decrementing counter for ", ip);
                        arp_cache.arpMap[ip] = ent
                        retry_arp_req(ip, ent.vlanid, ent.ifType, ent.localIP)
                    } else if (arp.counter == (timeout_counter - retry_cnt)) &&
                               arp.valid == false {
                        logger.Println("2. Deleting entry ", ip, " from Arp cache")
                        delete(arp_cache.arpMap, ip)
                    } else if arp.counter != 0 {
                        ent := arp_cache.arpMap[ip]
                        cnt = arp.counter
                        cnt--
                        ent.counter = cnt
                        //logger.Println("3. Decrementing counter for ", ip);
                        arp_cache.arpMap[ip] = ent
                    } else {
                        dbCmd = fmt.Sprintf(`DELETE FROM ARPCache WHERE key='%s' ;`, ip)
                        logger.Println(dbCmd)
                        if dbHdl != nil {
                            logger.Println("Executing DB Command:", dbCmd)
                            _, err = dbutils.ExecuteSQLStmt(dbCmd, dbHdl)
                            if err != nil {
                                logWriter.Err(fmt.Sprintln("Failed to Delete ARP entries from DB for %s %s", ip, err))
                            }
                        } else {
                            logger.Println("DB handler is nil");
                        }
                        logger.Println("3. Deleting entry ", ip, " from Arp cache")
                        delete(arp_cache.arpMap, ip)
                    }
                }
            } else if msg.msg_type == 3 {
                logger.Println("Received ARP response from neighbor...", msg.ip)
                ent := arp_cache.arpMap[msg.ip]
                ent.macAddr = msg.ent.macAddr
                ent.vlanid = msg.ent.vlanid
                ent.valid = msg.ent.valid
                ent.counter = msg.ent.counter
                ent.port    = msg.ent.port
                ent.ifName  = msg.ent.ifName
                ent.ifType  = msg.ent.ifType
                ent.localIP = msg.ent.localIP
                arp_cache.arpMap[msg.ip] = ent
                logger.Println("2. updateArpCache(): ", arp_cache.arpMap[msg.ip])
                err := storeArpTableInDB(int(ent.ifType), int(ent.vlanid), ent.ifName, int(ent.port), msg.ip, ent.localIP)
                if err != nil {
                    logWriter.Err("Unable to cache ARP Table in DB")
                }
                //3) Update asicd.
                if asicdClient.IsConnected {
                        logger.Println("2. Creating an entry in asic for IP:", msg.ip, "MAC:",
                                        (msg.ent.macAddr).String(), "VLAN:",
                                        (int32)(arp_cache.arpMap[msg.ip].vlanid))
                        rv, error := asicdClient.ClientHdl.CreateIPv4Neighbor(msg.ip,
                                             (msg.ent.macAddr).String(), (int32)(arp_cache.arpMap[msg.ip].vlanid),
                                             (int32)(msg.ent.port))
                        logWriter.Err(fmt.Sprintf("Asicd Create rv: ", rv, " error : ", error))
                } else {
                        logWriter.Err("2. Asicd client is not connected.")
                }
            } else {
                logger.Println("Invalid Msg type.")
                continue
            }
        }
}

func refresh_arp_entry(ip string, ifName string, localIP string) {
        logWriter.Err(fmt.Sprintln("Refresh ARP entry ", ifName))
        handle, err = pcap.OpenLive(ifName, snapshot_len, promiscuous, timeout_pcap)
        if handle == nil {
            logWriter.Err(fmt.Sprintln("Server: No device found.:device , err ", ifName, err))
            return
        }
        mac_addr, err := getMacAddrInterfaceName(ifName)
        if err != nil {
            logWriter.Err(fmt.Sprintln("Unable to get the MAC addr of ", ifName))
            return
        }
        logger.Println("MAC addr of ", ifName, ": ", mac_addr)
        sendArpReq(ip, handle, mac_addr, localIP)
        return
}

func retry_arp_req(ip string, vlanid arpd.Int, ifType arpd.Int, localIP string) {
        //logger.Println("Calling ResolveArpIPv4...", ip, " ", int32(ifType), " ", int32(vlanid))
//        var linux_device string
        if portdClient.IsConnected {
		linux_device, err := portdClient.ClientHdl.GetLinuxIfc(int32(ifType), int32(vlanid))
/*
                for _, port_cfg := range portCfgList {
                    linux_device = port_cfg.Ifname
*/
                    logger.Println("linux_device ", linux_device)
                    if err != nil {
                            logWriter.Err(fmt.Sprintf("Failed to get ifname for interface : ", vlanid, "type : ", ifType))
                            return
                    }
                    logWriter.Err(fmt.Sprintln("Server:Connecting to device ", linux_device))
                    handle, err = pcap.OpenLive(linux_device, snapshot_len, promiscuous, timeout_pcap)
                    if handle == nil {
                            logWriter.Err(fmt.Sprintln("Server: No device found.:device , err ", linux_device, err))
                            return
                    }
/*
                    mac_addr, err := getMacAddrInterfaceName(port_cfg.Ifname)
                    if err != nil {
                        logWriter.Err(fmt.Sprintln("Unable to get the MAC addr of ", port_cfg.Ifname))
                        continue
                    }
                    logger.Println("MAC addr of ", port_cfg.Ifname, ": ", mac_addr)
*/
                    mac_addr, err := getMacAddrInterfaceName(linux_device)
                    if err != nil {
                        logWriter.Err(fmt.Sprintln("Unable to get the MAC addr of ", linux_device))
                    }
                    logger.Println("MAC addr of ", linux_device, ": ", mac_addr)

                    sendArpReq(ip, handle, mac_addr, localIP)
/*
                }
*/

	} else {
		logWriter.Err("portd client is not connected.")
		logger.Println("Portd is not connected.")
	}
}

func printArpEntries() {
	logger.Println("************")
	for ip, arp := range arp_cache.arpMap {
		logger.Println("IP:", ip, " VLAN:", arp.vlanid, " MAC:", arp.macAddr, "CNT:", arp.counter, "PORT:", arp.port, "IfName:", arp.ifName, "IfType:", arp.ifType, "LocalIP:", arp.localIP, "Valid:", arp.valid)
	}
	logger.Println("************")
}

func timeout_thread() {
    for {
        time.Sleep(timeout)
        if dump_arp_table == true {
            logger.Println("===============Message from ARP Timeout Thread==============")
            printArpEntries()
            logger.Println("========================================================")
        }
        arp_cache_update_chl <- arpUpdateMsg {
                                    ip: "0",
                                    ent: arpEntry {
                                            macAddr: []byte{0, 0, 0, 0, 0, 0},
                                            vlanid: 0,
                                            valid: false,
                                            port: -1,
                                            ifName: "",
                                            ifType: -1,
                                            localIP: "",
                                            counter: timeout_counter,
                                         },
                                    msg_type: 2,
                                 }
    }
}