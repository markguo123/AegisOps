package k8s

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/k8s-ai/k8s-ai/internal/tools"
)

type resourceReader interface {
	Execute(context.Context, json.RawMessage) (tools.ToolResult, error)
}

type resourceContract struct {
	kind, resource, legacyGetDisable, legacyListDisable string
	cluster                                             bool
	read                                                func(base) resourceReader
}

var resourceContracts = []resourceContract{
	{"Pod", "/pods", "get_pod", "get_pods", false, func(b base) resourceReader { return &podTool{base: b} }},
	{"Node", "/nodes", "get_node", "get_nodes", true, func(b base) resourceReader { return &nodeTool{base: b} }},
	{"Deployment", "apps/deployments", "get_deployment", "", false, func(b base) resourceReader { return &deploymentTool{base: b} }},
	{"StatefulSet", "apps/statefulsets", "get_statefulset", "", false, func(b base) resourceReader { return &statefulSetTool{base: b} }},
	{"DaemonSet", "apps/daemonsets", "get_daemonset", "", false, func(b base) resourceReader { return &daemonSetTool{base: b} }},
	{"ReplicaSet", "apps/replicasets", "get_replicaset", "", false, func(b base) resourceReader { return &replicaSetTool{base: b} }},
	{"Service", "/services", "get_service", "", false, func(b base) resourceReader { return &serviceTool{base: b} }},
	{"Endpoints", "/endpoints", "get_endpoints", "", false, func(b base) resourceReader { return &endpointsTool{base: b} }},
	{"EndpointSlice", "discovery.k8s.io/endpointslices", "", "get_endpointslices", false, nil},
	{"PersistentVolumeClaim", "/persistentvolumeclaims", "get_pvc", "", false, func(b base) resourceReader { return &pvcTool{base: b} }},
	{"PersistentVolume", "/persistentvolumes", "get_pv", "", true, func(b base) resourceReader { return &pvTool{base: b} }},
	{"StorageClass", "storage.k8s.io/storageclasses", "get_storageclass", "", true, func(b base) resourceReader { return &storageClassTool{base: b} }},
	{"Namespace", "/namespaces", "get_namespace", "", true, func(b base) resourceReader { return &namespaceTool{base: b} }},
	{"ConfigMap", "/configmaps", "get_configmap", "get_configmaps", false, func(b base) resourceReader { return &configMapTool{base: b} }},
	{"Event", "/events", "", "", false, nil},
}

type resourcePageReader interface {
	ListResourcePage(context.Context, string, string, metav1.ListOptions) (runtime.Object, error)
}

func registerResourceCatalog(reg *tools.Registry, reader Reader, cluster string, disabled map[string]bool) error {
	var routes []tools.KindRoute
	var contracts []resourceContract
	b := base{reader: reader}
	// Legacy names here are configuration keys only.
	for _, c := range resourceContracts {
		if c.read != nil && !disabled["get_resource"] && !disabled[c.legacyGetDisable] {
			routes = append(routes, tools.KindRoute{Kind: c.kind, Execute: c.read(b).Execute, Capability: readCapability(c.resource), NamespaceRequired: !c.cluster && c.kind != "Pod", ClusterScoped: c.cluster})
		}
		dedicated := c.legacyListDisable != "" && !disabled[c.legacyListDisable]
		generic := c.kind != "ConfigMap" && !disabled["get_resources"]
		if dedicated || generic {
			contracts = append(contracts, c)
		}
	}
	if len(routes) > 0 {
		if err := reg.Register(tools.NewKindTool("get_resource", "Get the status or configuration summary of a known Kubernetes resource.", routes)); err != nil {
			return err
		}
	}
	if pages, ok := reader.(resourcePageReader); ok && !disabled["list_resources"] && len(contracts) > 0 {
		return reg.Register(&resourceListTool{base: b, pages: pages, contracts: contracts, cluster: cluster})
	}
	return nil
}

type resourceListTool struct {
	base
	pages     resourcePageReader
	contracts []resourceContract
	cluster   string
}

func (*resourceListTool) Name() string { return "list_resources" }
func (*resourceListTool) Description() string {
	return "List a bounded page of Kubernetes resources. Reuse continue with the identical query to read the next page. Use get_events for the most recent events."
}
func (t *resourceListTool) InputSchema() any {
	return t.schema(nil)
}

func (t *resourceListTool) ScopedInputSchema(scope tools.AccessScope) (any, bool) {
	schema := t.schema(&scope)
	return schema, schema != nil
}

func (t *resourceListTool) schema(scope *tools.AccessScope) any {
	kinds := make([]string, 0, len(t.contracts))
	for _, c := range t.contracts {
		if scope == nil || scope.Advertises(readCapability(c.resource)) {
			kinds = append(kinds, c.kind)
		}
	}
	if len(kinds) == 0 {
		return nil
	}
	return objSchema(map[string]any{
		"kind":          map[string]any{"type": "string", "enum": kinds},
		"namespace":     strProp("Namespace; required when access is namespace-scoped"),
		"labelSelector": strProp("Kubernetes label selector"),
		"fieldSelector": strProp("Kubernetes metadata.name or metadata.namespace selector"),
		"service":       strProp("EndpointSlice only; filter by Service name"),
		"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "default": 50},
		"continue":      strProp("Continuation token from the previous page"),
		"ownerKind":     strProp("Pod only: Deployment, StatefulSet, DaemonSet or ReplicaSet"),
		"ownerUID":      strProp("Pod only: UID of an observed controller; required with ownerKind"),
	}, "kind")
}

type resourceQuery struct {
	contract            resourceContract
	namespace           string
	ownerKind, ownerUID string
	opts                metav1.ListOptions
}

func (t *resourceListTool) parse(input json.RawMessage) (resourceQuery, error) {
	var q resourceQuery
	args, err := tools.ParseArgs(input, "kind")
	if err != nil {
		return q, err
	}
	for _, c := range t.contracts {
		if c.kind == args["kind"] {
			q.contract = c
			break
		}
	}
	if q.contract.kind == "" {
		return q, fmt.Errorf("unsupported kind %q", args["kind"])
	}
	q.namespace = args["namespace"]
	q.ownerKind, q.ownerUID = args["ownerKind"], args["ownerUID"]
	if q.ownerKind != "" || q.ownerUID != "" {
		if q.contract.kind != "Pod" || q.ownerUID == "" || q.namespace == "" {
			return q, fmt.Errorf("owner filtering requires kind=Pod, namespace, ownerKind and ownerUID")
		}
		switch q.ownerKind {
		case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet":
		default:
			return q, fmt.Errorf("unsupported Pod owner kind")
		}
	}
	if q.contract.cluster && q.namespace != "" {
		return q, fmt.Errorf("%s is cluster scoped", q.contract.kind)
	}
	labelSelector, err := labels.Parse(args["labelSelector"])
	if err != nil {
		return q, fmt.Errorf("invalid labelSelector: %w", err)
	}
	if service := args["service"]; service != "" {
		if q.contract.kind != "EndpointSlice" {
			return q, fmt.Errorf("service is supported only for EndpointSlice")
		}
		if len(validation.IsDNS1035Label(service)) != 0 {
			return q, fmt.Errorf("invalid Service name")
		}
		serviceSelector := labels.SelectorFromSet(labels.Set{"kubernetes.io/service-name": service})
		requirements, _ := serviceSelector.Requirements()
		labelSelector = labelSelector.Add(requirements...)
	}
	fieldSelector, err := fields.ParseSelector(args["fieldSelector"])
	if err != nil {
		return q, fmt.Errorf("invalid fieldSelector: %w", err)
	}
	for _, r := range fieldSelector.Requirements() {
		if r.Field != "metadata.name" && (r.Field != "metadata.namespace" || q.contract.cluster) {
			return q, fmt.Errorf("unsupported field selector %q", r.Field)
		}
	}
	limit := int64(50)
	if args["limit"] != "" {
		limit, err = strconv.ParseInt(args["limit"], 10, 64)
		if err != nil || limit < 1 || limit > 200 {
			return q, fmt.Errorf("limit must be between 1 and 200")
		}
	}
	q.opts = metav1.ListOptions{Limit: limit, LabelSelector: labelSelector.String(), FieldSelector: fieldSelector.String()}
	if token := args["continue"]; token != "" {
		q.opts.Continue, err = t.decodeCursor(q, token)
		if err != nil {
			return q, err
		}
	}
	return q, nil
}
func (t *resourceListTool) OperationFacts(input json.RawMessage) (tools.OperationFacts, error) {
	q, err := t.parse(input)
	if err != nil {
		return tools.OperationFacts{}, fmt.Errorf("INVALID_ARGUMENT: %w", err)
	}
	facts, err := readCapability(q.contract.resource).Facts(input, tools.AccessScope{})
	if err != nil || q.ownerKind != "Deployment" {
		return facts, err
	}
	replicaSets, err := readCapability("apps/replicasets").Facts(input, tools.AccessScope{})
	return tools.OperationFacts{Targets: append(facts.Targets, replicaSets.Targets...)}, err
}
func (t *resourceListTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	q, err := t.parse(input)
	if err != nil {
		return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, err.Error()), nil
	}
	result, err := t.pages.ListResourcePage(ctx, q.contract.resource, q.namespace, q.opts)
	if err != nil {
		return readError(err), nil
	}
	metadata, err := meta.ListAccessor(result)
	if err != nil {
		return tools.ToolResult{}, err
	}
	objects, err := meta.ExtractList(result)
	if err != nil {
		return tools.ToolResult{}, err
	}
	owners, err := t.podOwnerFilter(ctx, q)
	if err != nil {
		return readError(err), nil
	}
	items := make([]map[string]any, 0, len(objects))
	for _, object := range objects {
		metadata, err := meta.Accessor(object)
		if err != nil {
			return tools.ToolResult{}, err
		}
		if q.ownerUID != "" && !matchesController(metadata.GetOwnerReferences(), owners) {
			continue
		}
		data := resourceListSummary(object)
		data["name"], data["namespace"] = metadata.GetName(), metadata.GetNamespace()
		data["uid"], data["resourceVersion"] = metadata.GetUID(), metadata.GetResourceVersion()
		if owners := metadata.GetOwnerReferences(); len(owners) != 0 {
			data["ownerReferences"] = owners
		}
		items = append(items, data)
	}
	data := map[string]any{"kind": q.contract.kind + "List", "count": len(items), "items": items,
		"namespace": q.namespace, "clusterId": t.cluster, "limit": q.opts.Limit, "hasMore": metadata.GetContinue() != ""}
	if q.ownerUID != "" {
		data["sourcePageCount"] = len(objects)
		data["owner"] = map[string]string{"kind": q.ownerKind, "uid": q.ownerUID}
	}
	if next := metadata.GetContinue(); next != "" {
		data["continue"] = t.encodeCursor(q, next)
	}
	return tools.ToolResult{Data: data, Text: fmt.Sprintf("%s page: %d items", q.contract.kind, len(items))}, nil
}
func (t *resourceListTool) podOwnerFilter(ctx context.Context, q resourceQuery) (map[string]string, error) {
	if q.ownerKind != "Deployment" {
		return map[string]string{q.ownerUID: q.ownerKind}, nil
	}
	replicas, err := t.reader.ListReplicaSets(ctx, q.namespace)
	if err != nil {
		return nil, err
	}
	owners := map[string]string{}
	deployment := map[string]string{q.ownerUID: "Deployment"}
	for _, replica := range replicas {
		if replica.UID != "" && matchesController(replica.OwnerReferences, deployment) {
			owners[string(replica.UID)] = "ReplicaSet"
		}
	}
	return owners, nil
}

func matchesController(refs []metav1.OwnerReference, owners map[string]string) bool {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller && ref.UID != "" && owners[string(ref.UID)] == ref.Kind {
			return true
		}
	}
	return false
}

func resourceListSummary(object runtime.Object) map[string]any {
	switch value := object.(type) {
	case *corev1.Pod:
		item := *value
		return summarizePodListItem(item)
	case *corev1.Node:
		item := *value
		return summarizeNodeListItem(item)
	case *discoveryv1.EndpointSlice:
		item := *value
		return summarizeEndpointSlice(item)
	case *corev1.ConfigMap:
		item := *value
		return map[string]any{"dataKeyCount": len(item.Data), "binaryKeyCount": len(item.BinaryData)}
	case *corev1.Event:
		item := *value
		return summarizeEvent(item)
	default:
		return map[string]any{}
	}
}

var cursorKey = readCursorKey()

func readCursorKey() [32]byte {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic(err)
	}
	return key
}
func (t *resourceListTool) cursorSignature(q resourceQuery, token string) []byte {
	scope, _ := json.Marshal([]any{t.cluster, q.contract.resource, q.namespace, q.opts.LabelSelector, q.opts.FieldSelector, q.opts.Limit, q.ownerKind, q.ownerUID, token})
	mac := hmac.New(sha256.New, cursorKey[:])
	mac.Write(scope)
	return mac.Sum(nil)
}
func (t *resourceListTool) encodeCursor(q resourceQuery, token string) string {
	return base64.RawURLEncoding.EncodeToString(append(t.cursorSignature(q, token), token...))
}
func (t *resourceListTool) decodeCursor(q resourceQuery, cursor string) (string, error) {
	value, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(value) <= sha256.Size {
		return "", fmt.Errorf("invalid continuation token")
	}
	token := string(value[sha256.Size:])
	if !hmac.Equal(value[:sha256.Size], t.cursorSignature(q, token)) {
		return "", fmt.Errorf("continuation token does not match this query or has expired")
	}
	return token, nil
}
