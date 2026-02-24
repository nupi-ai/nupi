package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var grpcMethodRoles = map[string][]tokenRole{
	"/nupi.api.v1.AdapterRuntimeService/BindAdapter":       {roleAdmin},
	"/nupi.api.v1.AdapterRuntimeService/GetAdapterLogs":    {roleAdmin, roleReadOnly},
	"/nupi.api.v1.AdapterRuntimeService/Overview":          {roleAdmin, roleReadOnly},
	"/nupi.api.v1.AdapterRuntimeService/RegisterAdapter":   {roleAdmin},
	"/nupi.api.v1.AdapterRuntimeService/StartAdapter":      {roleAdmin},
	"/nupi.api.v1.AdapterRuntimeService/StopAdapter":       {roleAdmin},
	"/nupi.api.v1.AdapterRuntimeService/StreamAdapterLogs": {roleAdmin, roleReadOnly},
	"/nupi.api.v1.AudioService/GetAudioCapabilities":       {roleAdmin, roleReadOnly},
	"/nupi.api.v1.AudioService/InterruptTTS":               {roleAdmin},
	"/nupi.api.v1.AudioService/StreamAudioIn":              {roleAdmin},
	"/nupi.api.v1.AudioService/StreamAudioOut":             {roleAdmin},
	"/nupi.api.v1.AuthService/CreatePairing":               {roleAdmin},
	"/nupi.api.v1.AuthService/CreateToken":                 {roleAdmin},
	"/nupi.api.v1.AuthService/DeleteToken":                 {roleAdmin},
	"/nupi.api.v1.AuthService/ListPairings":                {roleAdmin},
	"/nupi.api.v1.AuthService/ListTokens":                  {roleAdmin},
	"/nupi.api.v1.AuthService/RegisterPushToken":           {roleAdmin},
	"/nupi.api.v1.AuthService/UnregisterPushToken":         {roleAdmin},
	"/nupi.api.v1.ConfigService/GetTransportConfig":        {roleAdmin},
	"/nupi.api.v1.ConfigService/Migrate":                   {roleAdmin},
	"/nupi.api.v1.ConfigService/UpdateTransportConfig":     {roleAdmin},
	"/nupi.api.v1.DaemonService/ReloadPlugins":             {roleAdmin},
	"/nupi.api.v1.DaemonService/Shutdown":                  {roleAdmin},
	"/nupi.api.v1.RecordingsService/GetRecording":          {roleAdmin, roleReadOnly},
	"/nupi.api.v1.RecordingsService/ListRecordings":        {roleAdmin, roleReadOnly},
	"/nupi.api.v1.SessionsService/AttachSession":           {roleAdmin, roleReadOnly},
	"/nupi.api.v1.SessionsService/CreateSession":           {roleAdmin},
	"/nupi.api.v1.SessionsService/GetConversation":         {roleAdmin, roleReadOnly},
	"/nupi.api.v1.SessionsService/GetGlobalConversation":   {roleAdmin, roleReadOnly},
	"/nupi.api.v1.SessionsService/GetSession":              {roleAdmin, roleReadOnly},
	"/nupi.api.v1.SessionsService/GetSessionMode":          {roleAdmin, roleReadOnly},
	"/nupi.api.v1.SessionsService/KillSession":             {roleAdmin},
	"/nupi.api.v1.SessionsService/ListSessions":            {roleAdmin, roleReadOnly},
	"/nupi.api.v1.SessionsService/SendInput":               {roleAdmin},
	"/nupi.api.v1.SessionsService/SendVoiceCommand":        {roleAdmin},
	"/nupi.api.v1.SessionsService/SetSessionMode":          {roleAdmin},
}

// grpcTokenOnlyMethods lists methods that require a valid auth token but no
// specific role. Any authenticated caller can invoke these.
var grpcTokenOnlyMethods = map[string]struct{}{
	"/nupi.api.v1.AdaptersService/ClearAdapterBinding": {},
	"/nupi.api.v1.AdaptersService/ListAdapterBindings": {},
	"/nupi.api.v1.AdaptersService/ListAdapters":        {},
	"/nupi.api.v1.AdaptersService/SetAdapterBinding":   {},
	"/nupi.api.v1.DaemonService/ListLanguages":         {},
	"/nupi.api.v1.DaemonService/Status":                {},
	"/nupi.api.v1.QuickstartService/GetStatus":         {},
	"/nupi.api.v1.QuickstartService/Update":            {},
}

// grpcPublicMethods lists methods that require no authentication at all.
// These are handled by the auth middleware (exempt from token validation).
var grpcPublicMethods = map[string]struct{}{
	"/nupi.api.v1.AuthService/ClaimPairing": {},
}

// AuthorizeGRPCMethod enforces method-level role requirements for gRPC calls.
// Uses default-deny: methods not listed in any authorization map are rejected.
func (s *APIServer) AuthorizeGRPCMethod(ctx context.Context, fullMethod string) error {
	if allowed, ok := grpcMethodRoles[fullMethod]; ok {
		_, err := s.requireRoleGRPC(ctx, allowed...)
		return err
	}
	if _, ok := grpcTokenOnlyMethods[fullMethod]; ok {
		return nil
	}
	if _, ok := grpcPublicMethods[fullMethod]; ok {
		return nil
	}
	return status.Errorf(codes.PermissionDenied, "no authorization policy for method %s", fullMethod)
}
