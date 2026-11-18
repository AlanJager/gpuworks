package extapi

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Every wire type of protocol major 1 and of each schema-1 object, with the
// JSON name and Go type of every field. The list is frozen: a field that
// changes what the server enforces means a new version of its object (a new
// type, a new golden directory), never an edit here. Adding a field to an
// existing type is refused by every older reader that decodes it strictly,
// so it is a new version too. Only Display contents may grow freely.
var frozenWireFields = `
AvailabilityRow.display Display
AvailabilityRow.down_access string
AvailabilityRow.feature string
AvailabilityRow.steps []AvailabilityStep
AvailabilityStep.access string
AvailabilityStep.at *Time
AvailabilityStep.limits []Limit
AvailabilityStep.link string
AvailabilityStep.notice *Notice
AvailabilityV1.down_grace_s int
AvailabilityV1.rows []AvailabilityRow
Attestation.blob []uint8
Attestation.feature string
Attestation.format string
Attestation.name string
AttestationsV1.attestations []Attestation
Banner.audience string
Banner.display Display
Banner.id string
Banner.steps []BannerStep
BannerStep.at *Time
BannerStep.hidden bool
BannerStep.link string
BannerStep.notice *Notice
BannerStep.severity string
BannersV1.banners []Banner
CRL.created Time
CRL.format int
CRL.publisher string
CRL.revoked []Revocation
CRL.serial uint64
CapacityStep.at *Time
CapacityStep.max_cards *int64
CapacityStep.max_nodes_with_cards *int64
CertGrants.authority_id string
CertGrants.feature_ns []string
CertGrants.id_prefixes []string
CertGrants.min_isolation string
CertGrants.roles []string
CertGrants.tables []string
CertGrants.ui_module bool
Clock.extclock Time
Clock.wall Time
ClockCorrected.from Time
ClockCorrected.to Time
ClockCorrection.reason string
ClockCorrection.to Time
CoreInfo.arch string
CoreInfo.ha_role string
CoreInfo.install_id string
CoreInfo.offer map[string]Range
CoreInfo.os string
CoreInfo.version string
DerivedKey.key []uint8
Envelope.payload []uint8
Envelope.signatures []Signature
FeatureDecl.gate string
FeatureDecl.key string
FeatureDecl.presentation string
FileEntry.exec bool
FileEntry.path string
FileEntry.sha256 string
FileEntry.size int64
FleetCapacityV1.hold Notice
FleetCapacityV1.keep []int64
FleetCapacityV1.refuse_card Notice
FleetCapacityV1.refuse_node Notice
FleetCapacityV1.select string
FleetCapacityV1.steps []CapacityStep
Floor.id string
Floor.min_version string
HandshakeRequest.clock Clock
HandshakeRequest.core CoreInfo
HandshakeRequest.held []HeldState
HandshakeRequest.plugin PluginGrant
HandshakeRequest.principal_key []uint8
HandshakeRequest.protocol Range
HandshakeResponse.id string
HandshakeResponse.manifest_sha256 Digest
Index.created Time
Index.floors []Floor
Index.format int
Index.packages []IndexEntry
Index.publisher string
Index.revoked []Revocation
Index.serial uint64
IndexEntry.display Display
IndexEntry.files []IndexFile
IndexEntry.id string
IndexEntry.manifest Digest
IndexEntry.min_core_hint string
IndexEntry.protocol Range
IndexEntry.requires map[string]int
IndexEntry.version string
IndexFile.path string
IndexFile.platform string
IndexFile.sha256 string
IndexFile.size int64
HandshakeResponse.protocol int
HandshakeResponse.services []string
HandshakeResponse.use map[string]int
HandshakeResponse.version string
Health.code string
Health.params Params
Health.status string
HeldState.digest Digest
HeldState.generation uint64
HeldState.kind string
HeldState.schema int
HostSetupStep.product_id string
HostSetupStep.type string
HostSetupStep.vendor_id string
HostMeta.boot_id string
HostMeta.extclock Time
HostMeta.hwm *Time
HostMeta.version string
HostMeta.wall Time
KeyRequest.label string
KeyRequest.length int
Limit.dim string
Limit.max *int64
Limit.unlimited bool
Manifest.codes []string
Manifest.created Time
Manifest.display Display
Manifest.features []FeatureDecl
Manifest.files []FileEntry
Manifest.host_setup []HostSetupStep
Manifest.id string
Manifest.min_core_hint string
Manifest.node *NodeDecl
Manifest.permissions []string
Manifest.protocol Range
Manifest.publisher string
Manifest.requires map[string]int
Manifest.roles []string
Manifest.routes []RouteDecl
Manifest.runtime *RuntimeDecl
Manifest.schema int
Manifest.settings []SettingDecl
Manifest.storage *StorageDecl
Manifest.ui *UIDecl
Manifest.uses map[string]int
Manifest.version string
NavDecl.feature string
NavDecl.group string
NavDecl.id string
NavDecl.label_key string
NavDecl.path string
NodeDecl.app string
NodeDecl.images []string
NodeHoldsV1.nodes map[string]Notice
Notice.code string
Notice.params Params
PluginGrant.authority_namespaces []string
PluginGrant.id string
PluginGrant.permissions_granted []string
PluginGrant.roles_granted []string
PluginGrant.version string
Principal.api_key_id uint64
Principal.exp Time
Principal.kind string
Principal.local_admin bool
Principal.platform_admin bool
Principal.request_id string
Principal.roles []string
Principal.route string
Principal.route_action string
Principal.scope_projects []string
Principal.user_id uint64
Principal.username string
PublicKey.alg string
PublicKey.keyid string
PublicKey.public_key []uint8
PublisherCert.alg string
PublisherCert.display Display
PublisherCert.format int
PublisherCert.grants CertGrants
PublisherCert.keyid string
PublisherCert.not_after Time
PublisherCert.not_before Time
PublisherCert.public_key []uint8
PublisherCert.publisher string
PublisherCert.serial uint64
PushedState.body RawMessage
PushedState.digest Digest
PushedState.generation uint64
PushedState.kind string
PushedState.pushed_at *Time
PushedState.schema int
Range.max int
Range.min int
Revocation.cert_serial uint64
Revocation.display Display
Revocation.keyid string
Revocation.manifest Digest
Revocation.publisher string
Revocation.replacement string
RouteDecl.access string
RouteDecl.action string
RouteDecl.ai_readable bool
RouteDecl.cookies bool
RouteDecl.feature string
RouteDecl.html bool
RouteDecl.max_body int64
RouteDecl.method string
RouteDecl.pattern string
RouteDecl.rate *RouteRate
RouteDecl.stream bool
RouteDecl.timeout_s int
RouteDecl.ungated bool
RouteDecl.websocket bool
RouteDecl.write_when_read_only bool
RouteRate.burst int
RouteRate.per_minute int
RuntimeDecl.cpu_milli int
RuntimeDecl.devices []string
RuntimeDecl.lifecycle string
RuntimeDecl.max_concurrency int
RuntimeDecl.mem_mib int
SettingDecl.key string
SettingDecl.label_key string
SettingDecl.max *int64
SettingDecl.max_length int
SettingDecl.min *int64
SettingDecl.required bool
SettingDecl.secret bool
SettingDecl.type string
SettingDecl.values []string
Signature.keyid string
Signature.sig []uint8
StateAck.applied_at *Time
StateAck.digest Digest
StateAck.generation uint64
StateAck.kind string
StateAck.superseded_by uint64
StatePush.base uint64
StatePush.body RawMessage
StatePush.generation uint64
StatePush.kind string
StatePush.schema int
StorageDecl.cold_copy_safe bool
StorageDecl.ha string
StorageDecl.kind string
StorageDecl.state_version int
TrustUpdate.add_roots []PublicKey
TrustUpdate.created Time
TrustUpdate.floors []Floor
TrustUpdate.format int
TrustUpdate.retire_roots []string
TrustUpdate.revoke []Revocation
TrustUpdate.serial uint64
UIDecl.entry string
UIDecl.mode string
UIDecl.nav []NavDecl
UIDecl.sdk Range
IdentityPart.h string
IdentityPart.tail string
InventoryCard.index int
InventoryCard.model string
InventoryCard.serial bool
InventoryNode.cards []InventoryCard
InventoryNode.enrolled_at Time
InventoryNode.host_reported bool
InventoryNode.id uint64
InventoryNode.name string
InventoryNode.uuid string
NodeIdentity.cards []IdentityPart
NodeIdentity.cards_without_serial int
NodeIdentity.host *IdentityPart
NodeIdentity.node_id uint64
NodeIdentityAnswer.nodes []NodeIdentity
NodeIdentityRequest.node_ids []uint64
NodeIdentityRequest.salt string
NodesPage.next string
NodesPage.nodes []InventoryNode
WireError.code string
WireError.message string
WireError.params Params
WireError.reason string
WireError.retry_after_ms int64
`

var wireRoots = []any{
	Manifest{}, PublisherCert{}, TrustUpdate{}, CRL{}, Index{}, Envelope{}, PublicKey{},
	HandshakeRequest{}, HandshakeResponse{}, Health{}, WireError{}, StatePush{}, StateAck{},
	NodeIdentityRequest{}, NodeIdentityAnswer{}, NodesPage{},
	ClockCorrection{}, ClockCorrected{}, HostMeta{}, PushedState{}, KeyRequest{}, DerivedKey{},
	AvailabilityV1{}, FleetCapacityV1{}, NodeHoldsV1{}, BannersV1{}, AttestationsV1{}, Principal{},
}

func TestWireFieldsAreFrozen(t *testing.T) {
	lines := map[string]bool{}
	seen := map[reflect.Type]bool{}
	for _, root := range wireRoots {
		describeFields(reflect.TypeOf(root), seen, lines)
	}
	var got []string
	for l := range lines {
		got = append(got, l)
	}
	sort.Strings(got)
	want := strings.Split(strings.TrimSpace(frozenWireFields), "\n")
	sort.Strings(want)
	wantSet := map[string]bool{}
	for _, l := range want {
		wantSet[l] = true
	}
	for _, l := range want {
		if !lines[l] {
			t.Errorf("frozen wire field removed or changed: %s", l)
		}
	}
	for _, l := range got {
		if !wantSet[l] {
			t.Errorf("wire field added: %s", l)
		}
	}
	if t.Failed() {
		t.Log("a field that changes what the server enforces is a new version of its object: a new type beside the old one and a new golden directory")
	}
}

func describeFields(t reflect.Type, seen map[reflect.Type]bool, out map[string]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] || t == timeType {
		return
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || name == "" || name == "-" {
			continue
		}
		out[fmt.Sprintf("%s.%s %s", t.Name(), name, typeName(f.Type))] = true
		describeFields(f.Type, seen, out)
	}
}

func typeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + typeName(t.Elem())
	case reflect.Slice:
		if t.Name() != "" {
			return t.Name()
		}
		return "[]" + typeName(t.Elem())
	case reflect.Map:
		if t.Name() != "" {
			return t.Name()
		}
		return "map[" + typeName(t.Key()) + "]" + typeName(t.Elem())
	}
	return t.Name()
}

// The table schemas a build reads only grow.
func TestTableSchemasAreFrozen(t *testing.T) {
	want := map[string][]int{
		KindAvailability: {1}, KindFleetCapacity: {1}, KindNodeHolds: {1}, KindBanners: {1}, KindAttestations: {1},
	}
	for kind, versions := range want {
		got := TableSchemas(kind)
		if len(got) < len(versions) {
			t.Errorf("%s: schemas %v lost a released version of %v", kind, got, versions)
			continue
		}
		for i, v := range versions {
			if got[i] != v {
				t.Errorf("%s: schemas %v do not start with released %v", kind, got, versions)
			}
		}
		if ClassOf(kind) == 0 {
			t.Errorf("%s: no class", kind)
		}
	}
	if ProtocolMajor != 1 || ManifestSchema != 1 || CertFormat != 1 || TrustUpdateFormat != 1 || CRLFormat != 1 {
		t.Fatal("a released version constant changed; newer versions are added beside the old ones")
	}
}
