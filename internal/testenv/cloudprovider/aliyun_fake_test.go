package cloudprovider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"
)

// fakeECS is an HTTP fixture of the Alibaba Cloud ECS and VPC RPC APIs. It
// answers in the real wire shapes (error body {Code, Message, RequestId},
// nested {X: {X: []}} lists, NextToken paging) and enforces what the real
// service enforces for these calls: the V3 signature headers, required
// parameters, image/zone/VPC consistency, the password policy, the system
// disk floor, CIDR containment and overlap, ClientToken idempotency, and the
// stock tables. Failure injection hooks sit on top of that, never instead.
type fakeECS struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// configuration
	regions    []string
	images     map[string]map[string]int // region -> image id -> minimum system disk GB
	spotStock  map[string]map[string]string
	odStock    map[string]map[string]string
	instTypes  map[string]bool
	pageSize   int
	runHook    func(f *fakeECS, p url.Values) (code, msg string, status int)
	listBroken map[string]string // region -> error code for DescribeInstances
	listFlaky  map[string]int    // region -> DescribeInstances calls still to fail with a 403

	// state
	seq     int
	created int
	// refuseDeletes makes the next DeleteInstance calls fail as Initializing.
	refuseDeletes int
	leakDisks     bool // a released instance leaves its system disk behind
	vpcs          map[string]*fakeVPC
	vswitches     map[string]*fakeVSwitch
	groups        map[string]*fakeGroup
	instances     map[string]*fakeInstance
	disks         map[string]*fakeDisk
	tokens        map[string]string
	calls         []fakeCall
	// bootAfter is how many DescribeInstances answers an instance stays Pending.
	bootAfter int
	// vanishAfter makes the instance of a given create ordinal (1-based)
	// disappear after that many describes, as a reclaimed spot instance does.
	vanishAfter map[int]int
}

type fakeCall struct {
	Product, Region, Action string
	Params                  url.Values
}

type fakeVPC struct{ ID, Name, CIDR, Region string }
type fakeVSwitch struct{ ID, Name, Zone, CIDR, VPC, Region string }
type fakeGroup struct {
	ID, Name, VPC, Region string
	Tags                  map[string]string
	Rules                 []map[string]string
}
type fakeInstance struct {
	ID, Name, Type, Region, Zone, Image, VSwitch, Group string
	Spot                                                bool
	Tags                                                map[string]string
	describes                                           int
	vanishAfter                                         int
	recycling                                           bool
	deleting                                            int
	disk                                                string
	publicIP, privateIP                                 string
	ordinal                                             int
}
type fakeDisk struct {
	ID, Region, Instance string
	Tags                 map[string]string
}

func newFakeECS(t *testing.T) *fakeECS {
	t.Helper()
	f := &fakeECS{
		t: t, regions: []string{"cn-alpha", "cn-beta", "us-gamma"},
		images:    map[string]map[string]int{"cn-alpha": {"m-alpha": 100}, "cn-beta": {"m-beta": 100}},
		spotStock: map[string]map[string]string{}, odStock: map[string]map[string]string{},
		instTypes: map[string]bool{}, pageSize: 2, listBroken: map[string]string{}, listFlaky: map[string]int{},
		vpcs: map[string]*fakeVPC{}, vswitches: map[string]*fakeVSwitch{}, groups: map[string]*fakeGroup{},
		instances: map[string]*fakeInstance{}, disks: map[string]*fakeDisk{}, tokens: map[string]string{},
		bootAfter: 2, vanishAfter: map[int]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// stock registers "zone|type" as sellable in a region for one mode.
func (f *fakeECS) stock(region string, spot bool, category string, zoneTypes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	table := f.odStock
	if spot {
		table = f.spotStock
	}
	if table[region] == nil {
		table[region] = map[string]string{}
	}
	for _, zt := range zoneTypes {
		table[region][zt] = category
	}
}

func (f *fakeECS) callsOf(action string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Action == action {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeECS) liveInstances() []*fakeInstance {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeInstance
	for _, inst := range f.instances {
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ordinal < out[j].ordinal })
	return out
}

func (f *fakeECS) next(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s-fake%04d", prefix, f.seq)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func apiFail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"Code": code, "Message": msg, "RequestId": "FAKE-REQUEST-ID", "HostId": "ecs.fake"})
}

func (f *fakeECS) serve(w http.ResponseWriter, r *http.Request) {
	action := r.Header.Get("x-acs-action")
	version := r.Header.Get("x-acs-version")
	auth := r.Header.Get("Authorization")
	if r.Method != http.MethodPost || action == "" || !strings.HasPrefix(auth, "ACS3-HMAC-SHA256 Credential=") ||
		!strings.Contains(auth, "SignedHeaders=") || !strings.Contains(auth, "Signature=") ||
		r.Header.Get("x-acs-signature-nonce") == "" || r.Header.Get("x-acs-date") == "" {
		apiFail(w, 400, "MissingParameter", "the V3 signature headers are required")
		return
	}
	product := "ecs"
	switch version {
	case "2014-05-26":
	case "2016-04-28":
		product = "vpc"
	default:
		apiFail(w, 400, "InvalidVersion", "unsupported API version "+version)
		return
	}
	q := r.URL.Query()
	region := q.Get("RegionId")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{Product: product, Region: region, Action: action, Params: q})
	if action != "DescribeRegions" && !f.knownRegion(region) {
		apiFail(w, 400, "InvalidRegionId.NotFound", "The specified RegionId does not exist.")
		return
	}
	handlers := map[string]func(http.ResponseWriter, url.Values, string){
		"ecs/DescribeRegions":                f.describeRegions,
		"ecs/DescribeAvailableResource":      f.describeAvailableResource,
		"ecs/DescribeSecurityGroups":         f.describeSecurityGroups,
		"ecs/CreateSecurityGroup":            f.createSecurityGroup,
		"ecs/DescribeSecurityGroupAttribute": f.describeSecurityGroupAttribute,
		"ecs/AuthorizeSecurityGroup":         f.authorizeSecurityGroup,
		"ecs/RunInstances":                   f.runInstances,
		"ecs/DescribeInstances":              f.describeInstances,
		"ecs/DeleteInstance":                 f.deleteInstance,
		"ecs/DescribeDisks":                  f.describeDisks,
		"ecs/DeleteDisk":                     f.deleteDisk,
		"vpc/DescribeVpcs":                   f.describeVpcs,
		"vpc/CreateVpc":                      f.createVpc,
		"vpc/DescribeVSwitches":              f.describeVSwitches,
		"vpc/CreateVSwitch":                  f.createVSwitch,
	}
	handler, ok := handlers[product+"/"+action]
	if !ok {
		apiFail(w, 400, "InvalidAction.NotFound", "Specified action "+action+" is not valid for "+product)
		return
	}
	handler(w, q, region)
}

func (f *fakeECS) knownRegion(region string) bool {
	for _, known := range f.regions {
		if known == region {
			return true
		}
	}
	return false
}

func (f *fakeECS) describeRegions(w http.ResponseWriter, _ url.Values, _ string) {
	var list []map[string]string
	for _, region := range f.regions {
		list = append(list, map[string]string{"RegionId": region})
	}
	writeJSON(w, 200, map[string]any{"RequestId": "R", "Regions": map[string]any{"Region": list}})
}

func (f *fakeECS) describeAvailableResource(w http.ResponseWriter, q url.Values, region string) {
	if q.Get("DestinationResource") != "InstanceType" || q.Get("ResourceType") != "instance" ||
		q.Get("InstanceChargeType") != "PostPaid" || q.Get("IoOptimized") != "optimized" {
		apiFail(w, 400, "MissingParameter", "DestinationResource, ResourceType, InstanceChargeType and IoOptimized are required")
		return
	}
	if s := q.Get("SpotStrategy"); s != "" && s != "SpotAsPriceGo" && s != "SpotWithPriceLimit" && s != "NoSpot" {
		apiFail(w, 400, "InvalidSpotStrategy", "The specified SpotStrategy is not valid.")
		return
	}
	table := f.odStock[region]
	if q.Get("SpotStrategy") == "SpotAsPriceGo" {
		table = f.spotStock[region]
	}
	byZone := map[string][]map[string]string{}
	for zt, category := range table {
		zone, typ, _ := strings.Cut(zt, "|")
		byZone[zone] = append(byZone[zone], map[string]string{"Value": typ, "Status": "Available", "StatusCategory": category})
	}
	// A sold-out type is listed too, the way the real API does.
	byZone["cn-sold-out"] = append(byZone["cn-sold-out"], map[string]string{"Value": "ecs.gn6i-c8g1.2xlarge", "Status": "SoldOut", "StatusCategory": "WithoutStock"})
	var zones []map[string]any
	for zone, supported := range byZone {
		zones = append(zones, map[string]any{
			"ZoneId": zone, "Status": "Available", "StatusCategory": "WithStock",
			"AvailableResources": map[string]any{"AvailableResource": []map[string]any{{
				"Type": "InstanceType", "SupportedResources": map[string]any{"SupportedResource": supported},
			}}},
		})
	}
	writeJSON(w, 200, map[string]any{"RequestId": "R", "RegionId": region, "AvailableZones": map[string]any{"AvailableZone": zones}})
}

func (f *fakeECS) describeVpcs(w http.ResponseWriter, q url.Values, region string) {
	var list []map[string]string
	for _, v := range f.vpcs {
		if v.Region != region || (q.Get("VpcName") != "" && v.Name != q.Get("VpcName")) || (q.Get("VpcId") != "" && v.ID != q.Get("VpcId")) {
			continue
		}
		list = append(list, map[string]string{"VpcId": v.ID, "VpcName": v.Name, "CidrBlock": v.CIDR, "Status": "Available"})
	}
	writeJSON(w, 200, map[string]any{"RequestId": "R", "TotalCount": len(list), "Vpcs": map[string]any{"Vpc": list}})
}

func (f *fakeECS) createVpc(w http.ResponseWriter, q url.Values, region string) {
	if q.Get("CidrBlock") == "" || q.Get("ClientToken") == "" {
		apiFail(w, 400, "MissingParameter.CidrBlock", "CidrBlock is mandatory for this action.")
		return
	}
	if id, ok := f.tokens["vpc/"+q.Get("ClientToken")]; ok {
		writeJSON(w, 200, map[string]string{"RequestId": "R", "VpcId": id})
		return
	}
	for _, v := range f.vpcs {
		if v.Region == region && v.Name == q.Get("VpcName") {
			apiFail(w, 400, "InvalidVpcName.Duplicated", "The specified VPC name already exists.")
			return
		}
	}
	id := f.next("vpc")
	f.vpcs[id] = &fakeVPC{ID: id, Name: q.Get("VpcName"), CIDR: q.Get("CidrBlock"), Region: region}
	f.tokens["vpc/"+q.Get("ClientToken")] = id
	writeJSON(w, 200, map[string]string{"RequestId": "R", "VpcId": id})
}

func (f *fakeECS) describeVSwitches(w http.ResponseWriter, q url.Values, region string) {
	if q.Get("VpcId") == "" {
		apiFail(w, 400, "MissingParameter", "VpcId is required here.")
		return
	}
	var list []map[string]string
	for _, v := range f.vswitches {
		if v.Region == region && v.VPC == q.Get("VpcId") {
			list = append(list, map[string]string{"VSwitchId": v.ID, "VSwitchName": v.Name, "ZoneId": v.Zone, "CidrBlock": v.CIDR, "Status": "Available"})
		}
	}
	writeJSON(w, 200, map[string]any{"RequestId": "R", "VSwitches": map[string]any{"VSwitch": list}})
}

func (f *fakeECS) createVSwitch(w http.ResponseWriter, q url.Values, region string) {
	vpc := f.vpcs[q.Get("VpcId")]
	if vpc == nil || vpc.Region != region {
		apiFail(w, 404, "InvalidVpcId.NotFound", "The specified VpcId does not exist.")
		return
	}
	if !strings.HasPrefix(q.Get("CidrBlock"), "172.30.") || !strings.HasSuffix(q.Get("CidrBlock"), ".0/24") {
		apiFail(w, 400, "InvalidCidrBlock.NotSubnet", "The specified CIDR block is not within the VPC CIDR block.")
		return
	}
	if id, ok := f.tokens["vsw/"+q.Get("ClientToken")]; ok {
		writeJSON(w, 200, map[string]string{"RequestId": "R", "VSwitchId": id})
		return
	}
	for _, v := range f.vswitches {
		if v.VPC == vpc.ID && v.CIDR == q.Get("CidrBlock") {
			apiFail(w, 400, "InvalidCidrBlock.Overlapped", "The specified CIDR block overlaps with another vSwitch.")
			return
		}
	}
	if q.Get("ZoneId") == "" || !strings.HasPrefix(q.Get("ZoneId"), region) {
		apiFail(w, 400, "InvalidZoneId.NotFound", "The specified zone does not belong to the region.")
		return
	}
	id := f.next("vsw")
	f.vswitches[id] = &fakeVSwitch{ID: id, Name: q.Get("VSwitchName"), Zone: q.Get("ZoneId"), CIDR: q.Get("CidrBlock"), VPC: vpc.ID, Region: region}
	f.tokens["vsw/"+q.Get("ClientToken")] = id
	writeJSON(w, 200, map[string]string{"RequestId": "R", "VSwitchId": id})
}

func (f *fakeECS) describeSecurityGroups(w http.ResponseWriter, q url.Values, region string) {
	var list []map[string]string
	for _, g := range f.groups {
		if g.Region == region && (q.Get("VpcId") == "" || g.VPC == q.Get("VpcId")) && (q.Get("SecurityGroupName") == "" || g.Name == q.Get("SecurityGroupName")) {
			list = append(list, map[string]string{"SecurityGroupId": g.ID, "SecurityGroupName": g.Name, "VpcId": g.VPC})
		}
	}
	writeJSON(w, 200, map[string]any{"RequestId": "R", "SecurityGroups": map[string]any{"SecurityGroup": list}})
}

func (f *fakeECS) createSecurityGroup(w http.ResponseWriter, q url.Values, region string) {
	vpc := f.vpcs[q.Get("VpcId")]
	if vpc == nil || vpc.Region != region {
		apiFail(w, 404, "InvalidVpcId.NotFound", "The specified VpcId does not exist.")
		return
	}
	if id, ok := f.tokens["sg/"+q.Get("ClientToken")]; ok {
		writeJSON(w, 200, map[string]string{"RequestId": "R", "SecurityGroupId": id})
		return
	}
	id := f.next("sg")
	f.groups[id] = &fakeGroup{ID: id, Name: q.Get("SecurityGroupName"), VPC: vpc.ID, Region: region, Tags: tagsOf(q)}
	f.tokens["sg/"+q.Get("ClientToken")] = id
	writeJSON(w, 200, map[string]string{"RequestId": "R", "SecurityGroupId": id})
}

func (f *fakeECS) describeSecurityGroupAttribute(w http.ResponseWriter, q url.Values, region string) {
	g := f.groups[q.Get("SecurityGroupId")]
	if g == nil || g.Region != region {
		apiFail(w, 404, "InvalidSecurityGroupId.NotFound", "The specified SecurityGroupId does not exist.")
		return
	}
	var perms []map[string]string
	for _, rule := range g.Rules {
		perm := map[string]string{"Direction": "ingress", "Policy": "accept", "IpProtocol": strings.ToUpper(rule["IpProtocol"]), "PortRange": rule["PortRange"]}
		if rule["SourceCidrIp"] != "" {
			perm["SourceCidrIp"] = rule["SourceCidrIp"]
		}
		if rule["SourceGroupId"] != "" {
			perm["SourceGroupId"] = rule["SourceGroupId"]
		}
		perms = append(perms, perm)
	}
	writeJSON(w, 200, map[string]any{"RequestId": "R", "SecurityGroupId": g.ID, "Permissions": map[string]any{"Permission": perms}})
}

func (f *fakeECS) authorizeSecurityGroup(w http.ResponseWriter, q url.Values, region string) {
	g := f.groups[q.Get("SecurityGroupId")]
	if g == nil || g.Region != region {
		apiFail(w, 404, "InvalidSecurityGroupId.NotFound", "The specified SecurityGroupId does not exist.")
		return
	}
	if q.Get("SourceGroupId") == g.ID {
		// The real service refuses this (and misspells "Parameter").
		apiFail(w, 403, "InvalidParamter.Conflict", "The specified SecurityGroupId should be different from the SourceGroupId.")
		return
	}
	protocol, ports := q.Get("IpProtocol"), q.Get("PortRange")
	valid := map[string]bool{"tcp": true, "udp": true, "icmp": true, "gre": true, "all": true}
	if !valid[protocol] || ports == "" || (q.Get("SourceCidrIp") == "") == (q.Get("SourceGroupId") == "") {
		apiFail(w, 400, "InvalidParameter", "IpProtocol, PortRange and exactly one of SourceCidrIp/SourceGroupId are required.")
		return
	}
	if protocol == "all" && ports != "-1/-1" {
		apiFail(w, 400, "InvalidPortRange.Malformed", "The specified PortRange is not valid for protocol ALL.")
		return
	}
	rule := map[string]string{"IpProtocol": protocol, "PortRange": ports, "SourceCidrIp": q.Get("SourceCidrIp"), "SourceGroupId": q.Get("SourceGroupId")}
	for _, existing := range g.Rules {
		if existing["IpProtocol"] == protocol && existing["PortRange"] == ports && existing["SourceCidrIp"] == rule["SourceCidrIp"] && existing["SourceGroupId"] == rule["SourceGroupId"] {
			apiFail(w, 400, "InvalidPermission.Duplicate", "The specified security group rule already exists.")
			return
		}
	}
	g.Rules = append(g.Rules, rule)
	writeJSON(w, 200, map[string]string{"RequestId": "R"})
}

func tagsOf(q url.Values) map[string]string {
	tags := map[string]string{}
	for i := 1; i <= 20; i++ {
		key := q.Get("Tag." + strconv.Itoa(i) + ".Key")
		if key == "" {
			break
		}
		tags[key] = q.Get("Tag." + strconv.Itoa(i) + ".Value")
	}
	return tags
}

// validPassword is ECS's policy: 8-30 characters with three of the four
// classes (upper, lower, digit, special from the documented set).
func validPassword(p string) bool {
	if len(p) < 8 || len(p) > 30 {
		return false
	}
	var upper, lower, digit, special bool
	for _, r := range p {
		switch {
		case unicode.IsUpper(r) && r < 128:
			upper = true
		case unicode.IsLower(r) && r < 128:
			lower = true
		case unicode.IsDigit(r):
			digit = true
		case strings.ContainsRune("()`~!@#$%^&*-_+=|{}[]:;'<>,.?/", r):
			special = true
		default:
			return false
		}
	}
	n := 0
	for _, ok := range []bool{upper, lower, digit, special} {
		if ok {
			n++
		}
	}
	return n >= 3
}

func (f *fakeECS) runInstances(w http.ResponseWriter, q url.Values, region string) {
	for _, required := range []string{"ImageId", "InstanceType", "SecurityGroupId", "VSwitchId", "ClientToken", "InstanceName"} {
		if q.Get(required) == "" {
			apiFail(w, 400, "MissingParameter", "The input parameter \""+required+"\" that is mandatory for processing this request is not supplied.")
			return
		}
	}
	minDisk, imageOK := f.images[region][q.Get("ImageId")]
	if !imageOK {
		apiFail(w, 404, "InvalidImageId.NotFound", "The specified ImageId does not exist.")
		return
	}
	if id, ok := f.tokens["run/"+q.Get("ClientToken")]; ok {
		writeJSON(w, 200, map[string]any{"RequestId": "R", "InstanceIdSets": map[string]any{"InstanceIdSet": []string{id}}})
		return
	}
	vsw := f.vswitches[q.Get("VSwitchId")]
	group := f.groups[q.Get("SecurityGroupId")]
	if vsw == nil || vsw.Region != region {
		apiFail(w, 404, "InvalidVSwitchId.NotFound", "The specified VSwitchId does not exist.")
		return
	}
	if group == nil || group.VPC != vsw.VPC {
		apiFail(w, 400, "InvalidSecurityGroupId.VpcMismatch", "The specified security group does not belong to the VPC of the vSwitch.")
		return
	}
	if q.Get("ZoneId") != vsw.Zone {
		apiFail(w, 400, "InvalidZoneId.NotFound", "The zone of the vSwitch does not match the specified ZoneId.")
		return
	}
	if !validPassword(q.Get("Password")) {
		apiFail(w, 400, "InvalidPassword.Malformed", "The specified parameter \"Password\" is not valid.")
		return
	}
	if size, err := strconv.Atoi(q.Get("SystemDisk.Size")); err != nil || size < minDisk {
		apiFail(w, 400, "InvalidSystemDiskSize.ValueNotSupported", "The specified SystemDisk.Size is smaller than the image size.")
		return
	}
	if q.Get("InstanceChargeType") != "PostPaid" || q.Get("InternetMaxBandwidthOut") == "" {
		apiFail(w, 400, "InvalidParameter", "InstanceChargeType must be PostPaid and a public bandwidth is required for a public IP.")
		return
	}
	spot := false
	switch q.Get("SpotStrategy") {
	case "":
	case "SpotAsPriceGo":
		spot = true
		if q.Get("SpotInterruptionBehavior") != "Terminate" {
			apiFail(w, 400, "InvalidParameter.SpotInterruptionBehavior", "SpotInterruptionBehavior must be Terminate.")
			return
		}
	default:
		apiFail(w, 400, "InvalidSpotStrategy.ValueNotSupported", "The specified SpotStrategy is not valid.")
		return
	}
	if f.runHook != nil {
		if code, msg, status := f.runHook(f, q); code != "" {
			apiFail(w, status, code, msg)
			return
		}
	}
	table := f.odStock[region]
	if spot {
		table = f.spotStock[region]
	}
	if _, inStock := table[q.Get("ZoneId")+"|"+q.Get("InstanceType")]; !inStock {
		apiFail(w, 400, "OperationDenied.NoStock", "The requested resource is sold out in the specified zone; try other types of resources or other regions and zones.")
		return
	}
	id := f.next("i")
	disk := f.next("d")
	inst := &fakeInstance{
		ID: id, Name: q.Get("InstanceName"), Type: q.Get("InstanceType"), Region: region, Zone: q.Get("ZoneId"), Image: q.Get("ImageId"),
		VSwitch: vsw.ID, Group: group.ID, Spot: spot, Tags: tagsOf(q), disk: disk, ordinal: f.seq,
		publicIP: "47.0.0." + strconv.Itoa(f.seq), privateIP: "172.30.1." + strconv.Itoa(f.seq),
	}
	f.created++
	if n, ok := f.vanishAfter[f.created]; ok {
		inst.vanishAfter = n
	}
	f.instances[id] = inst
	f.disks[disk] = &fakeDisk{ID: disk, Region: region, Instance: id, Tags: inst.Tags}
	f.tokens["run/"+q.Get("ClientToken")] = id
	writeJSON(w, 200, map[string]any{"RequestId": "R", "InstanceIdSets": map[string]any{"InstanceIdSet": []string{id}}})
}

func (f *fakeECS) describeInstances(w http.ResponseWriter, q url.Values, region string) {
	if f.listFlaky[region] > 0 {
		f.listFlaky[region]--
		apiFail(w, 403, "Forbidden.Temporary", "try again")
		return
	}
	if code := f.listBroken[region]; code != "" {
		apiFail(w, 403, code, "this region cannot be listed by the key")
		return
	}
	var ids map[string]bool
	if raw := q.Get("InstanceIds"); raw != "" {
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			apiFail(w, 400, "InvalidInstanceIds.Malformed", "InstanceIds must be a JSON array.")
			return
		}
		ids = map[string]bool{}
		for _, id := range list {
			ids[id] = true
		}
	}
	name := q.Get("InstanceName")
	var matched []*fakeInstance
	for _, inst := range f.instances {
		if inst.Region != region || (ids != nil && !ids[inst.ID]) {
			continue
		}
		if name != "" {
			if prefix, wildcard := strings.CutSuffix(name, "*"); wildcard {
				if !strings.HasPrefix(inst.Name, prefix) {
					continue
				}
			} else if inst.Name != name {
				continue
			}
		}
		matched = append(matched, inst)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ordinal < matched[j].ordinal })
	start := 0
	if token := q.Get("NextToken"); token != "" {
		start, _ = strconv.Atoi(token)
	}
	size := f.pageSize
	if max, err := strconv.Atoi(q.Get("MaxResults")); err == nil && max < size {
		size = max
	}
	end := min(start+size, len(matched))
	var out []map[string]any
	for _, inst := range matched[start:end] {
		inst.describes++
		if inst.vanishAfter > 0 && inst.describes > inst.vanishAfter {
			delete(f.instances, inst.ID)
			continue
		}
		if inst.deleting > 0 {
			inst.deleting--
			if inst.deleting == 0 {
				delete(f.instances, inst.ID)
				if !f.leakDisks {
					delete(f.disks, inst.disk)
				}
				continue
			}
		}
		status := "Pending"
		pub, priv := []string{}, []string{}
		if inst.describes > f.bootAfter {
			status = "Running"
			pub, priv = []string{inst.publicIP}, []string{inst.privateIP}
		}
		if inst.deleting > 0 {
			status = "Stopping"
		}
		row := map[string]any{
			"InstanceId": inst.ID, "InstanceName": inst.Name, "InstanceType": inst.Type, "RegionId": region, "ZoneId": inst.Zone,
			"Status": status, "SpotStrategy": map[bool]string{true: "SpotAsPriceGo", false: "NoSpot"}[inst.Spot],
			"GPUAmount": 1, "GPUSpec": "NVIDIA T4",
			"PublicIpAddress": map[string]any{"IpAddress": pub},
			"VpcAttributes":   map[string]any{"PrivateIpAddress": map[string]any{"IpAddress": priv}},
		}
		if inst.recycling {
			row["OperationLocks"] = map[string]any{"LockReason": []map[string]string{{"LockReason": "Recycling"}}}
		} else {
			row["OperationLocks"] = map[string]any{"LockReason": []map[string]string{}}
		}
		out = append(out, row)
	}
	resp := map[string]any{"RequestId": "R", "Instances": map[string]any{"Instance": out}}
	if end < len(matched) {
		resp["NextToken"] = strconv.Itoa(end)
	}
	writeJSON(w, 200, resp)
}

func (f *fakeECS) deleteInstance(w http.ResponseWriter, q url.Values, region string) {
	inst := f.instances[q.Get("InstanceId")]
	if inst == nil || inst.Region != region {
		apiFail(w, 404, "InvalidInstanceId.NotFound", "The specified InstanceId does not exist.")
		return
	}
	if f.refuseDeletes > 0 {
		// A just-started instance is still initializing its guest OS and
		// refuses deletion even with Force (observed on the real service).
		f.refuseDeletes--
		apiFail(w, 403, "IncorrectInstanceStatus.Initializing", "The specified instance status does not support this operation.")
		return
	}
	if q.Get("Force") != "true" {
		apiFail(w, 403, "IncorrectInstanceStatus", "A running instance must be stopped first, or deleted with Force=true.")
		return
	}
	inst.deleting = 2
	writeJSON(w, 200, map[string]string{"RequestId": "R"})
}

func (f *fakeECS) describeDisks(w http.ResponseWriter, q url.Values, region string) {
	var list []map[string]string
	for _, d := range f.disks {
		if d.Region != region || (q.Get("Tag.1.Key") != "" && d.Tags[q.Get("Tag.1.Key")] != q.Get("Tag.1.Value")) {
			continue
		}
		status := "In_use"
		if f.instances[d.Instance] == nil {
			status = "Available"
		}
		list = append(list, map[string]string{"DiskId": d.ID, "Status": status})
	}
	writeJSON(w, 200, map[string]any{"RequestId": "R", "Disks": map[string]any{"Disk": list}})
}

func (f *fakeECS) deleteDisk(w http.ResponseWriter, q url.Values, region string) {
	d := f.disks[q.Get("DiskId")]
	if d == nil || d.Region != region {
		apiFail(w, 404, "InvalidDiskId.NotFound", "The specified DiskId does not exist.")
		return
	}
	delete(f.disks, d.ID)
	writeJSON(w, 200, map[string]string{"RequestId": "R"})
}
