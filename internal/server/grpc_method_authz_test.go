package server

import (
	"sort"
	"strings"
	"testing"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"google.golang.org/grpc"
)

func TestGRPCMethodAuthorizationPolicyCoverage(t *testing.T) {
	allMethods := allGRPCMethods()

	var missing []string
	for method := range allMethods {
		if _, ok := grpcMethodRoles[method]; ok {
			continue
		}
		if _, ok := grpcTokenOnlyMethods[method]; ok {
			continue
		}
		if _, ok := grpcPublicMethods[method]; ok {
			continue
		}
		missing = append(missing, method)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("missing gRPC auth policy for methods: %s", strings.Join(missing, ", "))
	}

	assertMethodSetSubset(t, "grpcMethodRoles", keys(grpcMethodRoles), allMethods)
	assertMethodSetSubset(t, "grpcTokenOnlyMethods", keys(grpcTokenOnlyMethods), allMethods)
	assertMethodSetSubset(t, "grpcPublicMethods", keys(grpcPublicMethods), allMethods)
	assertNoOverlap(t, "grpcMethodRoles", keys(grpcMethodRoles), "grpcTokenOnlyMethods", keys(grpcTokenOnlyMethods))
	assertNoOverlap(t, "grpcMethodRoles", keys(grpcMethodRoles), "grpcPublicMethods", keys(grpcPublicMethods))
	assertNoOverlap(t, "grpcTokenOnlyMethods", keys(grpcTokenOnlyMethods), "grpcPublicMethods", keys(grpcPublicMethods))
}

func allGRPCMethods() map[string]struct{} {
	descs := []grpc.ServiceDesc{
		apiv1.DaemonService_ServiceDesc,
		apiv1.SessionsService_ServiceDesc,
		apiv1.AdapterRuntimeService_ServiceDesc,
		apiv1.AudioService_ServiceDesc,
		apiv1.AuthService_ServiceDesc,
		apiv1.RecordingsService_ServiceDesc,
		apiv1.ConfigService_ServiceDesc,
		apiv1.AdaptersService_ServiceDesc,
		apiv1.QuickstartService_ServiceDesc,
	}

	methods := make(map[string]struct{})
	for _, desc := range descs {
		for _, m := range desc.Methods {
			methods["/"+desc.ServiceName+"/"+m.MethodName] = struct{}{}
		}
		for _, m := range desc.Streams {
			methods["/"+desc.ServiceName+"/"+m.StreamName] = struct{}{}
		}
	}

	return methods
}

func keys[T any](m map[string]T) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

func assertMethodSetSubset(t *testing.T, setName string, set map[string]struct{}, allMethods map[string]struct{}) {
	t.Helper()

	var unknown []string
	for method := range set {
		if _, ok := allMethods[method]; !ok {
			unknown = append(unknown, method)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Fatalf("%s contains unknown methods: %s", setName, strings.Join(unknown, ", "))
	}
}

func assertNoOverlap(t *testing.T, leftName string, left map[string]struct{}, rightName string, right map[string]struct{}) {
	t.Helper()

	var overlap []string
	for method := range left {
		if _, ok := right[method]; ok {
			overlap = append(overlap, method)
		}
	}
	if len(overlap) > 0 {
		sort.Strings(overlap)
		t.Fatalf("%s overlaps with %s: %s", leftName, rightName, strings.Join(overlap, ", "))
	}
}
