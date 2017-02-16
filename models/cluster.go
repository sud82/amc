package models

import (
	"fmt"
	// "strconv"
	"errors"
	"html/template"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/Sirupsen/logrus"
	as "github.com/aerospike/aerospike-client-go"
	"github.com/kennygrant/sanitize"
	"github.com/mcuadros/go-version"
	"github.com/sasha-s/go-deadlock"
	"github.com/satori/go.uuid"

	"github.com/citrusleaf/amc/common"
	"github.com/citrusleaf/amc/mailer"
)

type Cluster struct {
	observer *ObserverT
	client   *as.Client
	nodes    map[as.Host]*Node

	// last time updated
	lastUpdate time.Time

	//pinged by user
	lastPing time.Time

	_datacenterInfo                      common.SyncStats
	aggNodeStats, aggNodeCalcStats       common.Stats
	aggNsStats, aggNsCalcStats           map[string]common.Stats
	aggTotalNsStats, aggTotalNsCalcStats common.Stats
	aggNsSetStats                        map[string]map[string]common.Stats // [namespace][set]aggregated stats
	jobs                                 atomic.Value                       // []common.Stats

	// either a uuid.V4, or a sorted comma delimited string of host:port
	uuid            string
	securityEnabled bool
	updateInterval  int // seconds

	seeds    []*as.Host
	alias    *string
	user     *string
	password *string // TODO: Keep hashed values only

	// Permanent clusters are loaded from the config file
	// They will not removed automatically after a period of inactivity
	permanent bool

	alerts *common.AlertBucket

	users                 []*as.UserRoles
	roles                 []*as.Role
	currentUserPrivileges []string

	activeBackup  *Backup
	activeRestore *Restore

	mutex deadlock.RWMutex
}

func newCluster(observer *ObserverT, client *as.Client, alias *string, user, password string, seeds []*as.Host) *Cluster {
	if alias != nil && len(*alias) == 0 {
		alias = nil
	}

	newCluster := Cluster{
		observer:        observer,
		client:          client,
		nodes:           map[as.Host]*Node{},
		alias:           alias,                              //seconds
		updateInterval:  observer.config.AMC.UpdateInterval, //seconds
		uuid:            uuid.NewV4().String(),
		seeds:           seeds,
		_datacenterInfo: *common.NewSyncStats(nil),
		alerts:          common.NewAlertBucket(50),
	}

	if user != "" {
		newCluster.user = &user
		newCluster.password = &password
	}

	if client != nil {
		nodes := client.GetNodes()
		for _, node := range nodes {
			newCluster.nodes[*node.GetHost()] = newNode(&newCluster, node)
		}
	}

	return &newCluster
}

func (c *Cluster) setPermanent(v bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.permanent = v
}

func (c *Cluster) closeAndUnset() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.client != nil {
		c.client.Close()
		c.client = nil
	}
}

func (c *Cluster) updateLastestPing() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.lastPing = time.Now()
}

func (c *Cluster) shouldAutoRemove() bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	// InactiveDurBeforeRemoval <= 0 means never remove
	return !c.permanent && c.observer.config.AMC.InactiveDurBeforeRemoval > 0 && time.Since(c.lastPing) > time.Duration(c.observer.config.AMC.InactiveDurBeforeRemoval)*time.Second
}

func (c *Cluster) AddNode(address string, port int) error {

	hostAddrList, err := resolveHost(address)
	if err != nil || len(hostAddrList) == 0 {
		return err
	}

	for _, address := range hostAddrList {
		host := as.NewHost(address, port)
		if _, exists := c.nodes[*host]; exists {
			return errors.New("Node already exists")
		}
	}

	host := as.NewHost(hostAddrList[0], port)

	// This is to make sure the client will have the seed for this node
	// In case ALL nodes are removed
	c.client.Cluster().AddSeeds([]*as.Host{host})

	// Node will be automatically assigned when available on cluster
	newNode := newNode(c, nil)
	newNode.origHost = host

	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.nodes[*host] = newNode

	return nil
}

func (c *Cluster) RemoveNodeByAddress(address string) error {
	node := c.FindNodeByAddress(address)
	if node == nil {
		return errors.New(fmt.Sprintf("Node %s not found.", address))
	}

	if node.Status() == nodeStatus.On {
		return errors.New(fmt.Sprintf("Node %s is active. Only inactive nodes can be removed.", address))
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	newNodes := make(map[as.Host]*Node, len(c.nodes))
	for host, oldNode := range c.nodes {
		if node != oldNode {
			newNodes[host] = oldNode
		}
	}

	c.nodes = newNodes
	return nil
}

func (c *Cluster) UpdateInterval() int {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return c.updateInterval
}

func (c *Cluster) SetUpdateInterval(val int) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.updateInterval = val
}

func (c *Cluster) OffNodes() []string {
	res := []string{}
	for _, node := range c.Nodes() {
		if node.Status() == nodeStatus.Off {
			res = append(res, node.Address())
		}
	}

	return res
}

func (c *Cluster) RandomActiveNode() *Node {
	for _, node := range c.Nodes() {
		if node.Status() == nodeStatus.On {
			return node
		}
	}

	return nil
}

func (c *Cluster) Status() string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if c.client != nil && c.client.IsConnected() {
		return "on"
	}
	return "off"
}

func (c *Cluster) Disk() common.Stats {
	result := common.Stats{
		"used": c.aggNodeCalcStats.TryInt("used-bytes-disk", 0),
		"free": c.aggNodeCalcStats.TryInt("free-bytes-disk", 0),
	}

	details := common.Stats{}
	for _, n := range c.Nodes() {
		details[n.Address()] = n.Disk()
	}

	result["details"] = details
	return result
}

func (c *Cluster) Memory() common.Stats {
	result := common.Stats{
		"used": c.aggNodeCalcStats.TryInt("used-bytes-memory", 0),
		"free": c.aggNodeCalcStats.TryInt("free-bytes-memory", 0),
	}

	details := common.Stats{}
	for _, n := range c.Nodes() {
		details[n.Address()] = n.Memory()
	}

	result["details"] = details
	return result
}

func (c *Cluster) Users() []*as.UserRoles {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	res := make([]*as.UserRoles, len(c.users))
	copy(res, c.users)
	return res
}

func (c *Cluster) UpdatePassword(user, currentPass, newPass string) error {
	if currentPass == newPass {
		return errors.New("New password cannot be same as current password")
	}

	if c.password != nil && currentPass != *c.password {
		return errors.New("Invalid current password")
	}

	if c.user != nil && user != *c.user {
		return errors.New("Invalid current user")
	}

	err := c.client.ChangePassword(nil, user, newPass)
	// update password
	if err == nil {
		c.password = &newPass
	}

	return err
}

func (c *Cluster) ChangeUserPassword(user, pass string) error {
	return c.client.ChangePassword(nil, user, pass)
}

func (c *Cluster) CreateUser(user, password string, roles []string) error {
	return c.client.CreateUser(nil, user, password, roles)
}

func (c *Cluster) DropUser(user string) error {
	return c.client.DropUser(nil, user)
}

func (c *Cluster) GrantRoles(user string, roles []string) error {
	return c.client.GrantRoles(nil, user, roles)
}

func (c *Cluster) RevokeRoles(user string, roles []string) error {
	return c.client.RevokeRoles(nil, user, roles)
}

func (c *Cluster) CreateRole(role string, privileges []as.Privilege) error {
	return c.client.CreateRole(nil, role, privileges)
}

func (c *Cluster) DropRole(role string) error {
	return c.client.DropRole(nil, role)
}

func (c *Cluster) AddPrivileges(role string, privileges []as.Privilege) error {
	return c.client.GrantPrivileges(nil, role, privileges)
}

func (c *Cluster) RemovePrivileges(role string, privileges []as.Privilege) error {
	return c.client.RevokePrivileges(nil, role, privileges)
}

func (c *Cluster) CreateUDF(name, body string) error {
	_, err := c.client.RegisterUDF(nil, []byte(body), name, as.LUA)
	return err
}

func (c *Cluster) DropUDF(udf string) error {
	_, err := c.client.RemoveUDF(nil, udf)
	return err
}

func (c *Cluster) CreateIndex(namespace, setName, indexName, binName, indexType string) error {
	_, err := c.client.CreateIndex(nil, namespace, setName, indexName, binName, as.IndexType(indexType))
	return err
}

func (c *Cluster) DropIndex(namespace, setName, indexName string) error {
	return c.client.DropIndex(nil, namespace, setName, indexName)
}

func (c *Cluster) Nodes() (nodes []*Node) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	for _, node := range c.nodes {
		nodes = append(nodes, node)
	}

	return nodes
}

func (c *Cluster) NodeBuilds() (builds []string) {
	for _, node := range c.Nodes() {
		builds = append(builds, node.Build())
	}

	return common.SortStrings(common.StrUniq(builds))
}

func (c *Cluster) NamespaceList() (result []string) {
	for _, node := range c.Nodes() {
		for _, ns := range node.NamespaceList() {
			result = append(result, ns)
		}
	}

	return common.SortStrings(common.StrUniq(result))
}

func (c *Cluster) NamespaceIndexes() map[string][]string {
	result := map[string][]string{}
	for _, node := range c.Nodes() {
		for ns, list := range node.NamespaceIndexes() {
			result[ns] = append(result[ns], list...)
		}
	}

	for k, v := range result {
		result[k] = common.StrUniq(v)
	}

	return result
}

func (c *Cluster) NodeList() []string {
	clusterNodes := c.Nodes()
	nodes := make([]string, 0, len(clusterNodes))
	for _, node := range clusterNodes {
		nodes = append(nodes, node.Address())
	}

	return common.SortStrings(nodes)
}

func (c *Cluster) NodeCompatibility() string {
	versionList := map[string][]string{}
	for _, node := range c.Nodes() {
		build := node.Build()
		versionList[build] = append(versionList[build], node.Address())
	}

	if len(versionList) <= 1 {
		return "homogeneous"
	}

	return "compatible"
}

func (c *Cluster) SeedAddress() string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return c.seeds[0].String()
}

func (c *Cluster) Id() string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return c.uuid
}

func (c *Cluster) User() *string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if c.user == nil || *c.user == "" {
		return nil
	}
	u := *c.user
	return &u
}

func (c *Cluster) Name() *string {
	for _, node := range c.Nodes() {
		if cName := node.ClusterName(); cName != "" && cName != "null" {
			return &cName
		}
	}

	return nil
}

func (c *Cluster) Alias() *string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if c.alias != nil && len(*c.alias) > 0 {
		alias := *c.alias
		return &alias
	}

	if cName := c.Name(); cName != nil {
		return cName
	}

	return nil
}

func (c *Cluster) SetAlias(alias string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if len(alias) == 0 {
		c.alias = nil
		return
	}

	c.alias = &alias
}

func (c *Cluster) Roles() []*as.Role {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if len(c.roles) == 0 {
		return nil
	}

	res := make([]*as.Role, len(c.roles))
	copy(res, c.roles)
	return res
}

func (c *Cluster) RoleNames() []string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if len(c.roles) == 0 {
		return []string{}
	}

	res := make([]string, 0, len(c.roles))
	for _, r := range c.roles {
		res = append(res, r.Name)
	}

	return common.SortStrings(res)
}

func (c *Cluster) close() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.client.Close()
	c.client = nil
}

func (c *Cluster) IsSet() bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return c.client != nil
}

func (c *Cluster) SecurityEnabled() bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return c.user != nil && len(*c.user) > 0
}

func (c *Cluster) shouldUpdate() bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return time.Since(c.lastUpdate) >= time.Second*time.Duration(c.updateInterval)
}

func (c *Cluster) setUpdatedAt(tm time.Time) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.lastUpdate = tm
}

func (c *Cluster) update(wg *sync.WaitGroup) error {
	// make sure panics do not bring the observer down
	defer func() {
		if err := recover(); err != nil {
			log.Error(debug.Stack())
		}
	}()

	if wg != nil {
		defer wg.Done()
	}
	defer func() { go c.SendEmailNotifications() }()

	if !c.IsSet() {
		return nil
	}

	// update only on update intervals
	if !c.shouldUpdate() {
		return nil
	}

	t := time.Now()
	c.updateCluster()
	c.updateStats()
	c.updateJobs()
	c.updateUsers()
	c.updateDatacenterInfo()
	c.checkHealth()
	log.Debugf("Updating stats for cluster took: %s", time.Since(t))

	c.setUpdatedAt(time.Now())

	return nil
}

func (c *Cluster) SendEmailNotifications() {
	newAlerts := c.alerts.DrainNewAlerts()

	// only try to send notifications if the mailer settings are set
	if len(c.observer.Config().Mailer.Host) == 0 {
		return
	}

	clusterName := c.Id()
	if alias := c.Alias(); alias != nil {
		clusterName = *alias
	}

	for _, alert := range newAlerts {
		// make the data structure, and send the mail
		msg := map[string]template.HTML{
			"Title":   template.HTML(fmt.Sprintf("Alert")),
			"Cluster": template.HTML(fmt.Sprintf("%s", clusterName)),
			"Node":    template.HTML(fmt.Sprintf("%s", alert.NodeAddress)),
			"Status":  template.HTML(fmt.Sprintf("<font color='%s'><strong>%s</strong></font>", alert.Status, strings.ToUpper(string(alert.Status)))),
			"Message": template.HTML(fmt.Sprintf("%s", alert.Desc)),
		}

		go func(context map[string]template.HTML) {
			for i := 0; i < 5; i++ {
				err := mailer.SendMail(c.observer.config, "alerts/generic.html", "AMC Alert: "+sanitize.HTML(string(context["Message"])), context)
				if err == nil {
					break
				}

				log.Errorf("Failed to send the notification email: %s", err.Error())
				time.Sleep(5 * time.Second)
			}
		}(msg)
	}
}

func (c *Cluster) checkHealth() error {
	return nil
}

func (c *Cluster) updateUsers() error {
	// update current user's privileges
	if c.user != nil && len(*c.user) > 0 {
		currentUserPrivileges := []string{}

		// this means the user do not have the privileges other than viewing its own roles
		if u, err := c.client.QueryUser(nil, *c.user); err == nil {
			for _, r := range u.Roles {
				role, err := c.client.QueryRole(nil, r)
				if err != nil {
					continue
				}

				for _, priv := range role.Privileges {
					currentUserPrivileges = append(currentUserPrivileges, string(priv.Code))
				}
			}

			c.currentUserPrivileges = currentUserPrivileges
		} else {
			return err
		}

	}

	users, _ := c.client.QueryUsers(nil)
	roles, _ := c.client.QueryRoles(nil)

	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.users = users
	c.roles = roles

	return nil
}

func (c *Cluster) RequestInfoAll(cmd string) (map[*Node]string, error) {
	type nodeCommand struct {
		Node *Node
		Res  map[string]string
		Err  error
	}

	nodes := c.Nodes()
	ch := make(chan nodeCommand, len(nodes))

	wg := new(sync.WaitGroup)
	for _, node := range nodes {
		if node != nil {
			wg.Add(1)
			go func(node *Node) {
				defer wg.Done()

				result, err := node.RequestInfo(1, cmd)
				ch <- nodeCommand{Node: node, Res: result, Err: err}
			}(node)
		} else {
			ch <- nodeCommand{Node: node, Res: nil, Err: nil}
		}
	}

	wg.Wait()
	close(ch)

	res := make(map[*Node]string, len(nodes))
	errsStr := []string{}
	for r := range ch {
		res[r.Node] = ""
		if r.Err != nil {
			errsStr = append(errsStr, r.Err.Error())
			res[r.Node] = r.Err.Error()
		} else if len(r.Res) > 0 && len(r.Res[cmd]) > 0 {
			res[r.Node] = r.Res[cmd]
		}
	}

	var err error
	if len(errsStr) > 0 {
		err = errors.New(strings.Join(errsStr, ", "))
	}

	return res, err
}

func (c *Cluster) registerNode(h *as.Host, n *Node) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.nodes[*h] = n
}

func (c *Cluster) updateCluster() error {
	for _, n := range c.client.GetNodes() {
		node := c.FindNodeByAddress(n.GetHost().String())
		if node == nil {
			node = c.FindNodeById(n.GetName())
		}

		if node != nil {
			if origNode := node.origNode(); origNode != n {
				if origNode != nil {
					origNode.Close()
				}
				node.setOrigNode(n)
			}
		} else {
			c.registerNode(n.GetHost(), newNode(c, n))
		}
	}

	return nil
}

func (c *Cluster) updateStats() error {
	// do the info calls in parallel
	wg := sync.WaitGroup{}
	wg.Add(len(c.nodes))
	for _, node := range c.nodes {
		go func(node *Node) {
			defer wg.Done()
			node.update()
		}(node)
	}
	wg.Wait()

	aggNodeStats := common.Stats{}
	aggNodeCalcStats := common.Stats{}
	aggNsStats := map[string]common.Stats{}
	aggNsCalcStats := map[string]common.Stats{}
	aggNsSetStats := map[string]map[string]common.Stats{}

	// then do the calculations synchronously, since they are fast and need synchronization anyway
	for _, node := range c.nodes {
		node.applyStatsToAggregate(aggNodeStats, aggNodeCalcStats)
		node.applyNsStatsToAggregate(aggNsStats, aggNsCalcStats)
		aggNsSetStats = node.applyNsSetStatsToAggregate(aggNsSetStats)
	}

	aggTotalNsStats := common.Stats{}
	for _, v := range aggNsStats {
		aggTotalNsStats.AggregateStats(v)
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.aggNodeStats = aggNodeStats
	c.aggNodeCalcStats = aggNodeCalcStats
	c.aggNsStats = aggNsStats
	c.aggNsCalcStats = aggNsCalcStats
	c.aggTotalNsStats = aggTotalNsStats
	c.aggNsSetStats = aggNsSetStats

	return nil
}

func (c *Cluster) versionSupported(oldest string) error {
	buildDetails := c.BuildDetails()
	verList := buildDetails["version_list"].(map[string][]string)

	for ver, nodeList := range verList {
		if version.Compare(ver, oldest, "<") {
			return errors.New(fmt.Sprintf("Database cluster is not supported. Latest supported version is: `v%s`. Nodes [%s] are at `v%s`", oldest, strings.Join(nodeList, ", "), ver))
		}
	}

	return nil
}

func (c *Cluster) BuildDetails() map[string]interface{} {
	result := map[string]interface{}{}

	versionList := map[string][]string{}
	latestBuild := ""
	for _, node := range c.Nodes() {
		build := node.Build()
		versionList[build] = append(versionList[build], node.Address())
		if version.Compare(build, latestBuild, ">") {
			latestBuild = build
		}
	}

	result["version_list"] = versionList
	result["latest_build_no"] = latestBuild

	c.updateLastestPing()
	return result
}

func (c *Cluster) LatestThroughput() map[string]map[string]*common.SinglePointValue {
	res := map[string]map[string]*common.SinglePointValue{}
	for _, node := range c.Nodes() {
		for statName, valueMap := range node.LatestThroughput() {
			if res[statName] == nil {
				res[statName] = valueMap
			} else {
				for nodeAddr, v := range valueMap {
					res[statName][nodeAddr] = v
				}
			}
		}
	}

	return res
}

func (c *Cluster) ServerTime() time.Time {
	var tm time.Time
	for _, node := range c.Nodes() {
		if tm.Before(node.ServerTime()) {
			tm = node.ServerTime()
		}
	}

	return tm
}

func (c *Cluster) ThroughputSince(tm time.Time) map[string]map[string][]*common.SinglePointValue {
	// if no tm specified, return for the last 30 mins
	if tm.IsZero() {
		tm = c.ServerTime().Add(-time.Minute * 30)
	}

	res := map[string]map[string][]*common.SinglePointValue{}
	for _, node := range c.Nodes() {
		for statName, valueMap := range node.ThroughputSince(tm) {
			if res[statName] == nil {
				res[statName] = valueMap
			} else {
				for k, v := range valueMap {
					res[statName][k] = v
				}
			}
		}
	}

	return res
}

func (c *Cluster) FindNodeById(id string) *Node {
	for _, node := range c.Nodes() {
		if node.Id() == id {
			return node
		}
	}

	return nil
}

func (c *Cluster) FindNodeByAddress(address string) *Node {
	for _, node := range c.Nodes() {
		if node.Address() == address {
			return node
		}
	}

	return nil
}

func (c *Cluster) FindNodesByAddress(addresses ...string) []*Node {
	res := make([]*Node, 0, len(addresses))
	for _, addr := range addresses {
		if node := c.FindNodeByAddress(addr); node != nil {
			res = append(res, node)
		}
	}

	return res
}

func (c *Cluster) NamespaceInfo(namespaces []string) map[string]common.Stats {
	res := make(map[string]common.Stats, len(namespaces))
	nodes := c.Nodes()
	for _, node := range nodes {
		for _, nsName := range namespaces {
			ns := node.NamespaceByName(nsName)
			if ns == nil {
				continue
			}

			nsStats := res[nsName]
			stats := ns.Stats()
			if nsStats == nil {
				nsStats = stats
			} else {
				nsStats.AggregateStats(stats)
			}

			leastDiskPct := map[string]interface{}{"node": nil, "value": nil}
			if availPct := stats.TryFloat("available_pct", -1); availPct >= 0 {
				if lpct := nsStats["least_available_pct"]; lpct != nil {
					leastDiskPct = lpct.(map[string]interface{})
				}
				if leastDiskPct["value"] == nil || availPct < leastDiskPct["value"].(float64) {
					leastDiskPct = map[string]interface{}{
						"node":  node.Address(),
						"value": availPct,
					}
				}
			}

			nsStats["master-objects-tombstones"] = fmt.Sprintf("%v / %v", common.Comma(nsStats.TryInt("master-objects", 0), ","), common.Comma(nsStats.TryInt("master_tombstones", 0), ","))
			nsStats["prole-objects-tombstones"] = fmt.Sprintf("%v / %v", common.Comma(nsStats.TryInt("prole-objects", 0), ","), common.Comma(nsStats.TryInt("prole_tombstones", 0), ","))

			nsStats["least_available_pct"] = leastDiskPct
			nsStats["cluster_status"] = c.Status()

			res[nsName] = nsStats
		}
	}

	for _, stats := range res {
		stats["repl-factor"] = stats.TryInt("repl-factor", 0) / int64(len(nodes))
	}

	return res
}

func (c *Cluster) NamespaceInfoPerNode(ns string, nodeAddrs []string) map[string]interface{} {
	res := make(map[string]interface{}, len(nodeAddrs))
	for _, nodeAddress := range nodeAddrs {
		node := c.FindNodeByAddress(nodeAddress)
		if node == nil {
			res[nodeAddress] = map[string]interface{}{
				"node_status": "off",
			}
			continue
		}

		ns := node.NamespaceByName(ns)
		if ns == nil {
			res[nodeAddress] = map[string]interface{}{
				"node_status": "off",
			}
			continue
		}

		nsStats := ns.StatsAttrs("master-objects", "master_tombstones", "prole-objects", "prole_tombstones")
		nodeInfo := common.Stats{
			"memory":                    ns.Memory(),
			"memory-pct":                ns.MemoryPercent(),
			"disk":                      ns.Disk(),
			"disk-pct":                  ns.DiskPercent(),
			"node_status":               node.Status(),
			"master-objects-tombstones": fmt.Sprintf("%v / %v", common.Comma(nsStats.TryInt("master-objects", 0), ","), common.Comma(nsStats.TryInt("master_tombstones", 0), ",")),
			"prole-objects-tombstones":  fmt.Sprintf("%v / %v", common.Comma(nsStats.TryInt("prole-objects", 0), ","), common.Comma(nsStats.TryInt("prole_tombstones", 0), ",")),
			"least_available_pct":       ns.StatsAttr("available_pct"),
		}

		subsetOfStats := []string{"expired-objects", "evicted-objects", "repl-factor",
			"memory-size", "free-pct-memory", "max-void-time", "hwm-breached",
			"default-ttl", "max-ttl", "max-ttl", "enable-xdr", "stop-writes",
			"available_pct", "stop-writes-pct", "hwm-breached", "single-bin",
			"data-in-memory", "type", "master-objects", "prole-objects",
			"master_tombstones", "prole_tombstones",
		}

		for k, v := range ns.StatsAttrs(subsetOfStats...) {
			nodeInfo[k] = v
		}

		res[nodeAddress] = nodeInfo
	}

	return res

}

func (c *Cluster) CurrentUserPrivileges() []string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return c.currentUserPrivileges
}

func (c *Cluster) NamespaceIndexInfo(namespace string) map[string]common.Info {
	for _, node := range c.Nodes() {
		if node.Status() == "on" {
			return node.Indexes(namespace)
		}
	}

	return map[string]common.Info{}
}

func (c *Cluster) NamespaceSetsInfo(namespace string) []common.Stats {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	attrs := []string{
		"delete", "deleting", "disable-eviction", "enable-xdr",
		"evict-hwm-count", "memory_data_bytes", "n_objects", "node_status",
		"ns", "ns_name", "objects", "set",
		"set_name", "stop-write-count", "stop-writes-count", "tombstones",
	}

	res := []common.Stats{}
	if setInfo := c.aggNsSetStats[namespace]; setInfo != nil {
		for _, v := range setInfo {
			res = append(res, v.GetMulti(attrs...))
		}
	}

	return res
}

func (c *Cluster) updateJobs() {
	res := []common.Stats{}
	for _, node := range c.Nodes() {
		for _, job := range node.Jobs() {
			job["node"] = common.Stats{
				"address":     node.Address(),
				"node_status": node.Status(),
				"build":       node.Build(),
				"memory":      node.Memory(),
			}

			res = append(res, job)
		}
	}

	c.jobs.Store(res)
}

func (c *Cluster) Jobs() []common.Stats {
	res := c.jobs.Load()
	if res == nil {
		return []common.Stats{}
	}

	return res.([]common.Stats)
}

func (c *Cluster) NamespaceDeviceInfo(namespace string) common.Stats {
	storageTypes := map[string][]string{}
	storageDevices := map[string][]string{}

	for _, node := range c.Nodes() {
		ns := node.NamespaceByName(namespace)
		storageType := ns.StatsAttr("type")
		if storageType != nil {
			storageTypes[storageType.(string)] = append(storageTypes[storageType.(string)], node.Address())
		}
		storageDevice := ns.StatsAttr("storage-engine")
		if storageDevice != nil {
			storageDevices[storageDevice.(string)] = append(storageDevices[storageDevice.(string)], node.Address())
		}
	}

	syncedStatus := len(storageTypes) <= 1
	return common.Stats{
		"cluster_status": "on",
		"synced":         syncedStatus,
		"storage":        storageTypes,
		"devices":        storageDevices,
	}
}

func (c *Cluster) updateDatacenterInfo() {
	c._datacenterInfo.SetStats(c.datacenterInfo())
}

func (c *Cluster) DatacenterInfo() common.Stats {
	return c._datacenterInfo.Clone()
}

func (c *Cluster) datacenterInfo() common.Stats {
	xdrInfo := map[string]common.Stats{}
	datacenterList := []string{}
	nodeStats := common.Stats{}
	remoteNodeStats := map[string]common.Stats{}
	for _, node := range c.Nodes() {
		dcs := node.DataCenters()
		for dcName, dcStats := range dcs {
			datacenterList = append(datacenterList, dcName)

			for _, nodeAddr := range dcStats["Nodes"].([]string) {
				remoteNodeStats[nodeAddr] = c.DiscoverDatacenter(dcStats)
				oldCluster := c.observer.NodeHasBeenDiscovered(nodeAddr)
				if oldCluster == nil {
					xdrInfo[nodeAddr] = common.Stats{"shipping_namespaces": dcStats["namespaces"].([]string)}
				} else {
					snIfc := xdrInfo[oldCluster.Id()]["shipping_namespaces"]
					if snIfc == nil {
						snIfc = []string{}
					}
					if xdrInfo[oldCluster.Id()] == nil {
						xdrInfo[oldCluster.Id()] = common.Stats{}
					}
					xdrInfo[oldCluster.Id()]["shipping_namespaces"] = common.StrUniq(append(snIfc.([]string), dcStats["namespaces"].([]string)...))
				}
			}
		}

		nodeStats[node.Id()] = common.Stats{
			"status":         node.Status(),
			"access_ip":      node.Host(),
			"access_port":    node.Port(),
			"ip":             node.Host(),
			"port":           node.Port(),
			"cur_throughput": 0,
			"xdr_uptime":     node.StatsAttr("xdr_uptime"),
			"lag":            node.StatsAttr("xdr_timelag"),
		}
	}

	readTotal, readSucc := 0.0, 0.0
	writeTotal, writeSucc := 0.0, 0.0
	zeroValue := float64(0)
	for stat, nodeMap := range c.LatestThroughput() {
		switch stat {
		case "stat_read_reqs":
			for _, v := range nodeMap {
				readTotal += *v.Value(&zeroValue)
			}
		case "stat_read_success":
			for _, v := range nodeMap {
				readSucc += *v.Value(&zeroValue)
			}
		case "stat_write_reqs":
			for _, v := range nodeMap {
				writeTotal += *v.Value(&zeroValue)
			}
		case "stat_write_success":
			for _, v := range nodeMap {
				writeSucc += *v.Value(&zeroValue)
			}
		}
	}

	return common.Stats{
		"seednode": c.SeedAddress(),
		"dc_name":  common.StrUniq(datacenterList),

		"xdr_info": xdrInfo,

		"cluster_name": c.Alias(),
		"namespaces":   c.NamespaceList(),
		"discovery":    "complete",
		"nodes":        nodeStats,
		"read_tps": common.Stats{
			"total":   readTotal,
			"success": readSucc,
		},
		"write_tps": common.Stats{
			"total":   writeTotal,
			"success": writeSucc,
		},

		"_remotes": remoteNodeStats,
	}
}

func (c *Cluster) DiscoverDatacenter(dc common.Stats) common.Stats {
	for _, nodeAddr := range dc["Nodes"].([]string) {
		host, port, err := common.SplitHostPort(nodeAddr)
		if err != nil {
			return nil
		}
		if c.observer.NodeHasBeenDiscovered(nodeAddr) == nil {
			return common.Stats{
				"dc_name":      []string{dc["DC_Name"].(string)},
				"discovery":    "secured", // TODO: think about this
				"seednode":     nodeAddr,
				"xdr_info":     common.Stats{},
				"cluster_name": nil,
				"namespaces":   []struct{}{},
				"nodes": common.Stats{
					nodeAddr: common.Stats{
						"status":         "off",
						"access_ip":      host,
						"cur_throughput": nil,
						"ip":             host,
						"access_port":    port,
						"xdr_uptime":     nil,
						"port":           port,
						"lag":            nil,
					},
				}, "read_tps": common.Stats{
					"total":   "0",
					"success": "0",
				},
				"write_tps": common.Stats{
					"total":   "0",
					"success": "0",
				},
			}
		}
	}
	return nil
}

func (c *Cluster) AlertsFrom(id int64) []*common.Alert {
	alerts := []*common.Alert{}
	for _, node := range c.Nodes() {
		alerts = append(alerts, node.AlertsFrom(id)...)
	}

	cid := c.Id()
	for _, alert := range alerts {
		alert.ClusterId = cid
	}

	return alerts
}

func (c *Cluster) Backup(
	Namespace string,
	DestinationAddress string,
	DestinationPath string,
	Username string,
	Password string,
	Sets string,
	MetadataOnly bool,
	TerminateOnChange bool,
	ScanPriority int) (*Backup, error) {

	if c.activeBackup != nil && c.activeBackup.Status == common.BackupStatusInProgress {
		return nil, errors.New("Another backup operation already exists and is in progress.")
	}

	c.activeBackup = &Backup{
		BackupRestore: common.NewBackupRestore(
			common.BackupRestoreTypeBackup,
			c.Id(),
			Namespace,
			DestinationAddress,
			Username,
			Password,
			DestinationPath,
			Sets,
			MetadataOnly,
			TerminateOnChange,
			ScanPriority,
			common.BackupStatusInProgress,
		),

		cluster: c,
	}

	if err := c.activeBackup.Save(); err != nil {
		c.activeBackup = nil
		return nil, err
	}

	// no need to set the activeBackup to nil, since it will be ignored in the condition above
	return c.activeBackup, c.activeBackup.Execute()
}

func (c *Cluster) CurrentBackup() *Backup {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return c.activeBackup
}

func (c *Cluster) Restore(
	Namespace string,
	DestinationAddress string,
	DestinationPath string,
	Username string,
	Password string,
	Threads int,
	MissingRecordsOnly bool,
	IgnoreGenerationNum bool) (*Restore, error) {

	if c.activeRestore != nil && c.activeRestore.Status == common.BackupStatusInProgress {
		return nil, errors.New("Another backup operation already exists and is in progress.")
	}

	c.activeRestore = &Restore{
		BackupRestore: common.NewBackupRestore(
			common.BackupRestoreTypeRestore,
			c.Id(),
			Namespace,
			DestinationAddress,
			Username,
			Password,
			DestinationPath,
			"",
			false,
			false,
			2,
			common.BackupStatusInProgress,
		),

		Threads:             Threads,
		MissingRecordsOnly:  MissingRecordsOnly,
		IgnoreGenerationNum: IgnoreGenerationNum,

		cluster: c,
	}

	if err := c.activeRestore.Save(); err != nil {
		c.activeRestore = nil
		return nil, err
	}

	// no need to set the activeRestore to nil, since it will be ignored in the condition above
	return c.activeRestore, c.activeRestore.Execute()
}

func (c *Cluster) CurrentRestore() *Restore {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return c.activeRestore

}
