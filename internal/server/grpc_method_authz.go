package server

import "context"

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
	"/nupi.api.v1.ConfigService/GetTransportConfig":        {roleAdmin},
	"/nupi.api.v1.ConfigService/Migrate":                   {roleAdmin},
	"/nupi.api.v1.ConfigService/UpdateTransportConfig":     {roleAdmin},
	"/nupi.api.v1.DaemonService/GetPluginWarnings":         {roleAdmin, roleReadOnly},
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

// AuthorizeGRPCMethod enforces method-level role requirements for gRPC calls.
// Methods absent from grpcMethodRoles are treated as token-only.
func (s *APIServer) AuthorizeGRPCMethod(ctx context.Context, fullMethod string) error {
	allowed, ok := grpcMethodRoles[fullMethod]
	if !ok {
		return nil
	}
	_, err := s.requireRoleGRPC(ctx, allowed...)
	return err
}
