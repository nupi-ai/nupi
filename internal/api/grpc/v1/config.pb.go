package v1

import (
	context "context"
	reflect "reflect"
	sync "sync"

	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	proto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/runtime/protoimpl"
	descriptorpb "google.golang.org/protobuf/types/descriptorpb"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
	wrapperspb "google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// Verify that this generated file is compatible with the proto package it is being compiled against.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that this generated file is compatible with the protoimpl package it is being compiled against.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

// TransportConfig represents daemon transport settings (HTTP + gRPC bindings, TLS, CORS).
type TransportConfig struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Binding        string   `protobuf:"bytes,1,opt,name=binding,proto3" json:"binding,omitempty"`
	Port           int32    `protobuf:"varint,2,opt,name=port,proto3" json:"port,omitempty"`
	TlsCertPath    string   `protobuf:"bytes,3,opt,name=tls_cert_path,json=tlsCertPath,proto3" json:"tls_cert_path,omitempty"`
	TlsKeyPath     string   `protobuf:"bytes,4,opt,name=tls_key_path,json=tlsKeyPath,proto3" json:"tls_key_path,omitempty"`
	AllowedOrigins []string `protobuf:"bytes,5,rep,name=allowed_origins,json=allowedOrigins,proto3" json:"allowed_origins,omitempty"`
	GrpcPort       int32    `protobuf:"varint,6,opt,name=grpc_port,json=grpcPort,proto3" json:"grpc_port,omitempty"`
	GrpcBinding    string   `protobuf:"bytes,7,opt,name=grpc_binding,json=grpcBinding,proto3" json:"grpc_binding,omitempty"`
	AuthRequired   bool     `protobuf:"varint,8,opt,name=auth_required,json=authRequired,proto3" json:"auth_required,omitempty"`
}

func (x *TransportConfig) Reset() {
	*x = TransportConfig{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_config_proto_msgTypes[0]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *TransportConfig) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*TransportConfig) ProtoMessage() {}

func (x *TransportConfig) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_config_proto_msgTypes[0]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *TransportConfig) GetBinding() string {
	if x != nil {
		return x.Binding
	}
	return ""
}

func (x *TransportConfig) GetPort() int32 {
	if x != nil {
		return x.Port
	}
	return 0
}

func (x *TransportConfig) GetTlsCertPath() string {
	if x != nil {
		return x.TlsCertPath
	}
	return ""
}

func (x *TransportConfig) GetTlsKeyPath() string {
	if x != nil {
		return x.TlsKeyPath
	}
	return ""
}

func (x *TransportConfig) GetAllowedOrigins() []string {
	if x != nil {
		return x.AllowedOrigins
	}
	return nil
}

func (x *TransportConfig) GetGrpcPort() int32 {
	if x != nil {
		return x.GrpcPort
	}
	return 0
}

func (x *TransportConfig) GetGrpcBinding() string {
	if x != nil {
		return x.GrpcBinding
	}
	return ""
}

func (x *TransportConfig) GetAuthRequired() bool {
	if x != nil {
		return x.AuthRequired
	}
	return false
}

type UpdateTransportConfigRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Config *TransportConfig `protobuf:"bytes,1,opt,name=config,proto3" json:"config,omitempty"`
}

func (x *UpdateTransportConfigRequest) Reset() {
	*x = UpdateTransportConfigRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_config_proto_msgTypes[1]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *UpdateTransportConfigRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*UpdateTransportConfigRequest) ProtoMessage() {}

func (x *UpdateTransportConfigRequest) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_config_proto_msgTypes[1]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *UpdateTransportConfigRequest) GetConfig() *TransportConfig {
	if x != nil {
		return x.Config
	}
	return nil
}

type Adapter struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Id        string `protobuf:"bytes,1,opt,name=id,proto3" json:"id,omitempty"`
	Source    string `protobuf:"bytes,2,opt,name=source,proto3" json:"source,omitempty"`
	Version   string `protobuf:"bytes,3,opt,name=version,proto3" json:"version,omitempty"`
	Type      string `protobuf:"bytes,4,opt,name=type,proto3" json:"type,omitempty"`
	Name      string `protobuf:"bytes,5,opt,name=name,proto3" json:"name,omitempty"`
	Manifest  string `protobuf:"bytes,6,opt,name=manifest,proto3" json:"manifest,omitempty"`
	CreatedAt string `protobuf:"bytes,7,opt,name=created_at,json=createdAt,proto3" json:"created_at,omitempty"`
	UpdatedAt string `protobuf:"bytes,8,opt,name=updated_at,json=updatedAt,proto3" json:"updated_at,omitempty"`
}

func (x *Adapter) Reset() {
	*x = Adapter{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_config_proto_msgTypes[2]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *Adapter) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Adapter) ProtoMessage() {}

func (x *Adapter) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_config_proto_msgTypes[2]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *Adapter) GetId() string {
	if x != nil {
		return x.Id
	}
	return ""
}

func (x *Adapter) GetSource() string {
	if x != nil {
		return x.Source
	}
	return ""
}

func (x *Adapter) GetVersion() string {
	if x != nil {
		return x.Version
	}
	return ""
}

func (x *Adapter) GetType() string {
	if x != nil {
		return x.Type
	}
	return ""
}

func (x *Adapter) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *Adapter) GetManifest() string {
	if x != nil {
		return x.Manifest
	}
	return ""
}

func (x *Adapter) GetCreatedAt() string {
	if x != nil {
		return x.CreatedAt
	}
	return ""
}

func (x *Adapter) GetUpdatedAt() string {
	if x != nil {
		return x.UpdatedAt
	}
	return ""
}

type ListAdaptersResponse struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Adapters []*Adapter `protobuf:"bytes,1,rep,name=adapters,proto3" json:"adapters,omitempty"`
}

func (x *ListAdaptersResponse) Reset() {
	*x = ListAdaptersResponse{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_config_proto_msgTypes[3]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *ListAdaptersResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ListAdaptersResponse) ProtoMessage() {}

func (x *ListAdaptersResponse) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_config_proto_msgTypes[3]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *ListAdaptersResponse) GetAdapters() []*Adapter {
	if x != nil {
		return x.Adapters
	}
	return nil
}

type AdapterBinding struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Slot      string `protobuf:"bytes,1,opt,name=slot,proto3" json:"slot,omitempty"`
	AdapterId string `protobuf:"bytes,2,opt,name=adapter_id,json=adapterId,proto3" json:"adapter_id,omitempty"`
	Status    string `protobuf:"bytes,3,opt,name=status,proto3" json:"status,omitempty"`
	Config    string `protobuf:"bytes,4,opt,name=config,proto3" json:"config,omitempty"`
	UpdatedAt string `protobuf:"bytes,5,opt,name=updated_at,json=updatedAt,proto3" json:"updated_at,omitempty"`
}

func (x *AdapterBinding) Reset() {
	*x = AdapterBinding{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_config_proto_msgTypes[4]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *AdapterBinding) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AdapterBinding) ProtoMessage() {}

func (x *AdapterBinding) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_config_proto_msgTypes[4]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *AdapterBinding) GetSlot() string {
	if x != nil {
		return x.Slot
	}
	return ""
}

func (x *AdapterBinding) GetAdapterId() string {
	if x != nil {
		return x.AdapterId
	}
	return ""
}

func (x *AdapterBinding) GetStatus() string {
	if x != nil {
		return x.Status
	}
	return ""
}

func (x *AdapterBinding) GetConfig() string {
	if x != nil {
		return x.Config
	}
	return ""
}

func (x *AdapterBinding) GetUpdatedAt() string {
	if x != nil {
		return x.UpdatedAt
	}
	return ""
}

type ListAdapterBindingsResponse struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Bindings []*AdapterBinding `protobuf:"bytes,1,rep,name=bindings,proto3" json:"bindings,omitempty"`
}

func (x *ListAdapterBindingsResponse) Reset() {
	*x = ListAdapterBindingsResponse{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_config_proto_msgTypes[5]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *ListAdapterBindingsResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ListAdapterBindingsResponse) ProtoMessage() {}

func (x *ListAdapterBindingsResponse) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_config_proto_msgTypes[5]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *ListAdapterBindingsResponse) GetBindings() []*AdapterBinding {
	if x != nil {
		return x.Bindings
	}
	return nil
}

type SetAdapterBindingRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Slot       string `protobuf:"bytes,1,opt,name=slot,proto3" json:"slot,omitempty"`
	AdapterId  string `protobuf:"bytes,2,opt,name=adapter_id,json=adapterId,proto3" json:"adapter_id,omitempty"`
	ConfigJson string `protobuf:"bytes,3,opt,name=config_json,json=configJson,proto3" json:"config_json,omitempty"`
}

func (x *SetAdapterBindingRequest) Reset() {
	*x = SetAdapterBindingRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_config_proto_msgTypes[6]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *SetAdapterBindingRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*SetAdapterBindingRequest) ProtoMessage() {}

func (x *SetAdapterBindingRequest) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_config_proto_msgTypes[6]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *SetAdapterBindingRequest) GetSlot() string {
	if x != nil {
		return x.Slot
	}
	return ""
}

func (x *SetAdapterBindingRequest) GetAdapterId() string {
	if x != nil {
		return x.AdapterId
	}
	return ""
}

func (x *SetAdapterBindingRequest) GetConfigJson() string {
	if x != nil {
		return x.ConfigJson
	}
	return ""
}

type ClearAdapterBindingRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Slot string `protobuf:"bytes,1,opt,name=slot,proto3" json:"slot,omitempty"`
}

func (x *ClearAdapterBindingRequest) Reset() {
	*x = ClearAdapterBindingRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_config_proto_msgTypes[7]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *ClearAdapterBindingRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ClearAdapterBindingRequest) ProtoMessage() {}

func (x *ClearAdapterBindingRequest) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_config_proto_msgTypes[7]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *ClearAdapterBindingRequest) GetSlot() string {
	if x != nil {
		return x.Slot
	}
	return ""
}

type QuickstartStatusResponse struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Completed    bool     `protobuf:"varint,1,opt,name=completed,proto3" json:"completed,omitempty"`
	CompletedAt  string   `protobuf:"bytes,2,opt,name=completed_at,json=completedAt,proto3" json:"completed_at,omitempty"`
	PendingSlots []string `protobuf:"bytes,3,rep,name=pending_slots,json=pendingSlots,proto3" json:"pending_slots,omitempty"`
}

func (x *QuickstartStatusResponse) Reset() {
	*x = QuickstartStatusResponse{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_config_proto_msgTypes[8]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *QuickstartStatusResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*QuickstartStatusResponse) ProtoMessage() {}

func (x *QuickstartStatusResponse) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_config_proto_msgTypes[8]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *QuickstartStatusResponse) GetCompleted() bool {
	if x != nil {
		return x.Completed
	}
	return false
}

func (x *QuickstartStatusResponse) GetCompletedAt() string {
	if x != nil {
		return x.CompletedAt
	}
	return ""
}

func (x *QuickstartStatusResponse) GetPendingSlots() []string {
	if x != nil {
		return x.PendingSlots
	}
	return nil
}

type QuickstartBinding struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Slot      string `protobuf:"bytes,1,opt,name=slot,proto3" json:"slot,omitempty"`
	AdapterId string `protobuf:"bytes,2,opt,name=adapter_id,json=adapterId,proto3" json:"adapter_id,omitempty"`
}

func (x *QuickstartBinding) Reset() {
	*x = QuickstartBinding{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_config_proto_msgTypes[9]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *QuickstartBinding) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*QuickstartBinding) ProtoMessage() {}

func (x *QuickstartBinding) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_config_proto_msgTypes[9]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *QuickstartBinding) GetSlot() string {
	if x != nil {
		return x.Slot
	}
	return ""
}

func (x *QuickstartBinding) GetAdapterId() string {
	if x != nil {
		return x.AdapterId
	}
	return ""
}

type UpdateQuickstartRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Bindings []*QuickstartBinding  `protobuf:"bytes,1,rep,name=bindings,proto3" json:"bindings,omitempty"`
	Complete *wrapperspb.BoolValue `protobuf:"bytes,2,opt,name=complete,proto3" json:"complete,omitempty"`
}

func (x *UpdateQuickstartRequest) Reset() {
	*x = UpdateQuickstartRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_config_proto_msgTypes[10]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *UpdateQuickstartRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*UpdateQuickstartRequest) ProtoMessage() {}

func (x *UpdateQuickstartRequest) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_config_proto_msgTypes[10]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *UpdateQuickstartRequest) GetBindings() []*QuickstartBinding {
	if x != nil {
		return x.Bindings
	}
	return nil
}

func (x *UpdateQuickstartRequest) GetComplete() *wrapperspb.BoolValue {
	if x != nil {
		return x.Complete
	}
	return nil
}

var File_nupi_api_config_proto protoreflect.FileDescriptor

var (
	file_nupi_api_config_proto_rawDescOnce sync.Once
	file_nupi_api_config_proto_rawDesc     []byte
)

func file_nupi_api_config_proto_rawDescGZIP() []byte {
	file_nupi_api_config_proto_rawDescOnce.Do(func() {
		file_nupi_api_config_proto_rawDesc = protoimpl.X.CompressGZIP(file_nupi_api_config_proto_rawDesc)
	})
	return file_nupi_api_config_proto_rawDesc
}

var file_nupi_api_config_proto_msgTypes = make([]protoimpl.MessageInfo, 11)
var file_nupi_api_config_proto_goTypes = []interface{}{
	(*TransportConfig)(nil),              // 0: nupi.api.v1.TransportConfig
	(*UpdateTransportConfigRequest)(nil), // 1: nupi.api.v1.UpdateTransportConfigRequest
	(*Adapter)(nil),                      // 2: nupi.api.v1.Adapter
	(*ListAdaptersResponse)(nil),         // 3: nupi.api.v1.ListAdaptersResponse
	(*AdapterBinding)(nil),               // 4: nupi.api.v1.AdapterBinding
	(*ListAdapterBindingsResponse)(nil),  // 5: nupi.api.v1.ListAdapterBindingsResponse
	(*SetAdapterBindingRequest)(nil),     // 6: nupi.api.v1.SetAdapterBindingRequest
	(*ClearAdapterBindingRequest)(nil),   // 7: nupi.api.v1.ClearAdapterBindingRequest
	(*QuickstartStatusResponse)(nil),     // 8: nupi.api.v1.QuickstartStatusResponse
	(*QuickstartBinding)(nil),            // 9: nupi.api.v1.QuickstartBinding
	(*UpdateQuickstartRequest)(nil),      // 10: nupi.api.v1.UpdateQuickstartRequest
	(*wrapperspb.BoolValue)(nil),         // 11: google.protobuf.BoolValue
	(*emptypb.Empty)(nil),                // 12: google.protobuf.Empty
}
var file_nupi_api_config_proto_depIdxs = []int32{
	0,  // 0: nupi.api.v1.UpdateTransportConfigRequest.config:type_name -> nupi.api.v1.TransportConfig
	2,  // 1: nupi.api.v1.ListAdaptersResponse.adapters:type_name -> nupi.api.v1.Adapter
	4,  // 2: nupi.api.v1.ListAdapterBindingsResponse.bindings:type_name -> nupi.api.v1.AdapterBinding
	9,  // 3: nupi.api.v1.UpdateQuickstartRequest.bindings:type_name -> nupi.api.v1.QuickstartBinding
	11, // 4: nupi.api.v1.UpdateQuickstartRequest.complete:type_name -> google.protobuf.BoolValue
	12, // 5: nupi.api.v1.ConfigService.GetTransportConfig:input_type -> google.protobuf.Empty
	0,  // 6: nupi.api.v1.ConfigService.GetTransportConfig:output_type -> nupi.api.v1.TransportConfig
	1,  // 7: nupi.api.v1.ConfigService.UpdateTransportConfig:input_type -> nupi.api.v1.UpdateTransportConfigRequest
	0,  // 8: nupi.api.v1.ConfigService.UpdateTransportConfig:output_type -> nupi.api.v1.TransportConfig
	12, // 9: nupi.api.v1.AdaptersService.ListAdapters:input_type -> google.protobuf.Empty
	3,  // 10: nupi.api.v1.AdaptersService.ListAdapters:output_type -> nupi.api.v1.ListAdaptersResponse
	12, // 11: nupi.api.v1.AdaptersService.ListAdapterBindings:input_type -> google.protobuf.Empty
	5,  // 12: nupi.api.v1.AdaptersService.ListAdapterBindings:output_type -> nupi.api.v1.ListAdapterBindingsResponse
	6,  // 13: nupi.api.v1.AdaptersService.SetAdapterBinding:input_type -> nupi.api.v1.SetAdapterBindingRequest
	4,  // 14: nupi.api.v1.AdaptersService.SetAdapterBinding:output_type -> nupi.api.v1.AdapterBinding
	7,  // 15: nupi.api.v1.AdaptersService.ClearAdapterBinding:input_type -> nupi.api.v1.ClearAdapterBindingRequest
	12, // 16: nupi.api.v1.AdaptersService.ClearAdapterBinding:output_type -> google.protobuf.Empty
	12, // 17: nupi.api.v1.QuickstartService.GetStatus:input_type -> google.protobuf.Empty
	8,  // 18: nupi.api.v1.QuickstartService.GetStatus:output_type -> nupi.api.v1.QuickstartStatusResponse
	10, // 19: nupi.api.v1.QuickstartService.Update:input_type -> nupi.api.v1.UpdateQuickstartRequest
	8,  // 20: nupi.api.v1.QuickstartService.Update:output_type -> nupi.api.v1.QuickstartStatusResponse
}

func init() { file_nupi_api_config_proto_init() }
func file_nupi_api_config_proto_init() {
	if File_nupi_api_config_proto != nil {
		return
	}
	fd := &descriptorpb.FileDescriptorProto{
		Syntax:  proto.String("proto3"),
		Name:    proto.String("nupi/api/v1/config.proto"),
		Package: proto.String("nupi.api.v1"),
		Dependency: []string{
			"google/protobuf/empty.proto",
			"google/protobuf/wrappers.proto",
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("TransportConfig"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("binding"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("port"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()},
					{Name: proto.String("tls_cert_path"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("tls_key_path"), Number: proto.Int32(4), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("allowed_origins"), Number: proto.Int32(5), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("grpc_port"), Number: proto.Int32(6), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()},
					{Name: proto.String("grpc_binding"), Number: proto.Int32(7), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("auth_required"), Number: proto.Int32(8), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()},
				},
			},
			{
				Name: proto.String("UpdateTransportConfigRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("config"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".nupi.api.v1.TransportConfig")},
				},
			},
			{
				Name: proto.String("Adapter"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("id"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("source"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("version"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("type"), Number: proto.Int32(4), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("name"), Number: proto.Int32(5), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("manifest"), Number: proto.Int32(6), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("created_at"), Number: proto.Int32(7), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("updated_at"), Number: proto.Int32(8), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				},
			},
			{
				Name: proto.String("ListAdaptersResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("adapters"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".nupi.api.v1.Adapter")},
				},
			},
			{
				Name: proto.String("AdapterBinding"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("slot"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("adapter_id"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("status"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("config"), Number: proto.Int32(4), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("updated_at"), Number: proto.Int32(5), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				},
			},
			{
				Name: proto.String("ListAdapterBindingsResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("bindings"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".nupi.api.v1.AdapterBinding")},
				},
			},
			{
				Name: proto.String("SetAdapterBindingRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("slot"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("adapter_id"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("config_json"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				},
			},
			{
				Name: proto.String("ClearAdapterBindingRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("slot"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				},
			},
			{
				Name: proto.String("QuickstartStatusResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("completed"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()},
					{Name: proto.String("completed_at"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("pending_slots"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				},
			},
			{
				Name: proto.String("QuickstartBinding"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("slot"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("adapter_id"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				},
			},
			{
				Name: proto.String("UpdateQuickstartRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("bindings"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".nupi.api.v1.QuickstartBinding")},
					{Name: proto.String("complete"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".google.protobuf.BoolValue")},
				},
			},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name: proto.String("ConfigService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{Name: proto.String("GetTransportConfig"), InputType: proto.String(".google.protobuf.Empty"), OutputType: proto.String(".nupi.api.v1.TransportConfig")},
					{Name: proto.String("UpdateTransportConfig"), InputType: proto.String(".nupi.api.v1.UpdateTransportConfigRequest"), OutputType: proto.String(".nupi.api.v1.TransportConfig")},
				},
			},
			{
				Name: proto.String("AdaptersService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{Name: proto.String("ListAdapters"), InputType: proto.String(".google.protobuf.Empty"), OutputType: proto.String(".nupi.api.v1.ListAdaptersResponse")},
					{Name: proto.String("ListAdapterBindings"), InputType: proto.String(".google.protobuf.Empty"), OutputType: proto.String(".nupi.api.v1.ListAdapterBindingsResponse")},
					{Name: proto.String("SetAdapterBinding"), InputType: proto.String(".nupi.api.v1.SetAdapterBindingRequest"), OutputType: proto.String(".nupi.api.v1.AdapterBinding")},
					{Name: proto.String("ClearAdapterBinding"), InputType: proto.String(".nupi.api.v1.ClearAdapterBindingRequest"), OutputType: proto.String(".google.protobuf.Empty")},
				},
			},
			{
				Name: proto.String("QuickstartService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{Name: proto.String("GetStatus"), InputType: proto.String(".google.protobuf.Empty"), OutputType: proto.String(".nupi.api.v1.QuickstartStatusResponse")},
					{Name: proto.String("Update"), InputType: proto.String(".nupi.api.v1.UpdateQuickstartRequest"), OutputType: proto.String(".nupi.api.v1.QuickstartStatusResponse")},
				},
			},
		},
	}

	rawDesc, err := proto.Marshal(fd)
	if err != nil {
		panic(err)
	}
	file_nupi_api_config_proto_rawDesc = rawDesc
	if !protoimpl.UnsafeEnabled {
		file_nupi_api_config_proto_msgTypes[0].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*TransportConfig); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_nupi_api_config_proto_msgTypes[1].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*UpdateTransportConfigRequest); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_nupi_api_config_proto_msgTypes[2].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*Adapter); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_nupi_api_config_proto_msgTypes[3].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*ListAdaptersResponse); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_nupi_api_config_proto_msgTypes[4].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*AdapterBinding); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_nupi_api_config_proto_msgTypes[5].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*ListAdapterBindingsResponse); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_nupi_api_config_proto_msgTypes[6].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*SetAdapterBindingRequest); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_nupi_api_config_proto_msgTypes[7].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*ClearAdapterBindingRequest); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_nupi_api_config_proto_msgTypes[8].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*QuickstartStatusResponse); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_nupi_api_config_proto_msgTypes[9].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*QuickstartBinding); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_nupi_api_config_proto_msgTypes[10].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*UpdateQuickstartRequest); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
	}

	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: rawDesc,
			NumEnums:      0,
			NumMessages:   11,
			NumExtensions: 0,
			NumServices:   3,
		},
		GoTypes:           file_nupi_api_config_proto_goTypes,
		DependencyIndexes: file_nupi_api_config_proto_depIdxs,
		MessageInfos:      file_nupi_api_config_proto_msgTypes,
	}.Build()

	File_nupi_api_config_proto = out.File
	file_nupi_api_config_proto_rawDesc = nil
	file_nupi_api_config_proto_goTypes = nil
	file_nupi_api_config_proto_depIdxs = nil
}

// gRPC client and server interfaces.

type ConfigServiceClient interface {
	GetTransportConfig(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*TransportConfig, error)
	UpdateTransportConfig(ctx context.Context, in *UpdateTransportConfigRequest, opts ...grpc.CallOption) (*TransportConfig, error)
}

type configServiceClient struct {
	cc grpc.ClientConnInterface
}

func NewConfigServiceClient(cc grpc.ClientConnInterface) ConfigServiceClient {
	return &configServiceClient{cc}
}

func (c *configServiceClient) GetTransportConfig(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*TransportConfig, error) {
	out := new(TransportConfig)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.ConfigService/GetTransportConfig", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *configServiceClient) UpdateTransportConfig(ctx context.Context, in *UpdateTransportConfigRequest, opts ...grpc.CallOption) (*TransportConfig, error) {
	out := new(TransportConfig)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.ConfigService/UpdateTransportConfig", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

type ConfigServiceServer interface {
	GetTransportConfig(context.Context, *emptypb.Empty) (*TransportConfig, error)
	UpdateTransportConfig(context.Context, *UpdateTransportConfigRequest) (*TransportConfig, error)
	mustEmbedUnimplementedConfigServiceServer()
}

type UnimplementedConfigServiceServer struct{}

func (UnimplementedConfigServiceServer) GetTransportConfig(context.Context, *emptypb.Empty) (*TransportConfig, error) {
	return nil, status.Errorf(codes.Unimplemented, "method GetTransportConfig not implemented")
}
func (UnimplementedConfigServiceServer) UpdateTransportConfig(context.Context, *UpdateTransportConfigRequest) (*TransportConfig, error) {
	return nil, status.Errorf(codes.Unimplemented, "method UpdateTransportConfig not implemented")
}
func (UnimplementedConfigServiceServer) mustEmbedUnimplementedConfigServiceServer() {}

func RegisterConfigServiceServer(s grpc.ServiceRegistrar, srv ConfigServiceServer) {
	s.RegisterService(&ConfigService_ServiceDesc, srv)
}

func _ConfigService_GetTransportConfig_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(emptypb.Empty)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(ConfigServiceServer).GetTransportConfig(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.ConfigService/GetTransportConfig",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(ConfigServiceServer).GetTransportConfig(ctx, req.(*emptypb.Empty))
	}
	return interceptor(ctx, in, info, handler)
}

func _ConfigService_UpdateTransportConfig_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(UpdateTransportConfigRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(ConfigServiceServer).UpdateTransportConfig(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.ConfigService/UpdateTransportConfig",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(ConfigServiceServer).UpdateTransportConfig(ctx, req.(*UpdateTransportConfigRequest))
	}
	return interceptor(ctx, in, info, handler)
}

var ConfigService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "nupi.api.v1.ConfigService",
	HandlerType: (*ConfigServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "GetTransportConfig",
			Handler:    _ConfigService_GetTransportConfig_Handler,
		},
		{
			MethodName: "UpdateTransportConfig",
			Handler:    _ConfigService_UpdateTransportConfig_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "nupi/api/v1/config.proto",
}

type AdaptersServiceClient interface {
	ListAdapters(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*ListAdaptersResponse, error)
	ListAdapterBindings(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*ListAdapterBindingsResponse, error)
	SetAdapterBinding(ctx context.Context, in *SetAdapterBindingRequest, opts ...grpc.CallOption) (*AdapterBinding, error)
	ClearAdapterBinding(ctx context.Context, in *ClearAdapterBindingRequest, opts ...grpc.CallOption) (*emptypb.Empty, error)
}

type adaptersServiceClient struct {
	cc grpc.ClientConnInterface
}

func NewAdaptersServiceClient(cc grpc.ClientConnInterface) AdaptersServiceClient {
	return &adaptersServiceClient{cc}
}

func (c *adaptersServiceClient) ListAdapters(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*ListAdaptersResponse, error) {
	out := new(ListAdaptersResponse)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.AdaptersService/ListAdapters", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *adaptersServiceClient) ListAdapterBindings(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*ListAdapterBindingsResponse, error) {
	out := new(ListAdapterBindingsResponse)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.AdaptersService/ListAdapterBindings", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *adaptersServiceClient) SetAdapterBinding(ctx context.Context, in *SetAdapterBindingRequest, opts ...grpc.CallOption) (*AdapterBinding, error) {
	out := new(AdapterBinding)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.AdaptersService/SetAdapterBinding", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *adaptersServiceClient) ClearAdapterBinding(ctx context.Context, in *ClearAdapterBindingRequest, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	out := new(emptypb.Empty)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.AdaptersService/ClearAdapterBinding", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

type AdaptersServiceServer interface {
	ListAdapters(context.Context, *emptypb.Empty) (*ListAdaptersResponse, error)
	ListAdapterBindings(context.Context, *emptypb.Empty) (*ListAdapterBindingsResponse, error)
	SetAdapterBinding(context.Context, *SetAdapterBindingRequest) (*AdapterBinding, error)
	ClearAdapterBinding(context.Context, *ClearAdapterBindingRequest) (*emptypb.Empty, error)
	mustEmbedUnimplementedAdaptersServiceServer()
}

type UnimplementedAdaptersServiceServer struct{}

func (UnimplementedAdaptersServiceServer) ListAdapters(context.Context, *emptypb.Empty) (*ListAdaptersResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ListAdapters not implemented")
}
func (UnimplementedAdaptersServiceServer) ListAdapterBindings(context.Context, *emptypb.Empty) (*ListAdapterBindingsResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ListAdapterBindings not implemented")
}
func (UnimplementedAdaptersServiceServer) SetAdapterBinding(context.Context, *SetAdapterBindingRequest) (*AdapterBinding, error) {
	return nil, status.Errorf(codes.Unimplemented, "method SetAdapterBinding not implemented")
}
func (UnimplementedAdaptersServiceServer) ClearAdapterBinding(context.Context, *ClearAdapterBindingRequest) (*emptypb.Empty, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ClearAdapterBinding not implemented")
}
func (UnimplementedAdaptersServiceServer) mustEmbedUnimplementedAdaptersServiceServer() {}

func RegisterAdaptersServiceServer(s grpc.ServiceRegistrar, srv AdaptersServiceServer) {
	s.RegisterService(&AdaptersService_ServiceDesc, srv)
}

func _AdaptersService_ListAdapters_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(emptypb.Empty)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AdaptersServiceServer).ListAdapters(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.AdaptersService/ListAdapters",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AdaptersServiceServer).ListAdapters(ctx, req.(*emptypb.Empty))
	}
	return interceptor(ctx, in, info, handler)
}

func _AdaptersService_ListAdapterBindings_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(emptypb.Empty)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AdaptersServiceServer).ListAdapterBindings(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.AdaptersService/ListAdapterBindings",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AdaptersServiceServer).ListAdapterBindings(ctx, req.(*emptypb.Empty))
	}
	return interceptor(ctx, in, info, handler)
}

func _AdaptersService_SetAdapterBinding_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(SetAdapterBindingRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AdaptersServiceServer).SetAdapterBinding(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.AdaptersService/SetAdapterBinding",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AdaptersServiceServer).SetAdapterBinding(ctx, req.(*SetAdapterBindingRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _AdaptersService_ClearAdapterBinding_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ClearAdapterBindingRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AdaptersServiceServer).ClearAdapterBinding(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.AdaptersService/ClearAdapterBinding",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AdaptersServiceServer).ClearAdapterBinding(ctx, req.(*ClearAdapterBindingRequest))
	}
	return interceptor(ctx, in, info, handler)
}

var AdaptersService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "nupi.api.v1.AdaptersService",
	HandlerType: (*AdaptersServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "ListAdapters",
			Handler:    _AdaptersService_ListAdapters_Handler,
		},
		{
			MethodName: "ListAdapterBindings",
			Handler:    _AdaptersService_ListAdapterBindings_Handler,
		},
		{
			MethodName: "SetAdapterBinding",
			Handler:    _AdaptersService_SetAdapterBinding_Handler,
		},
		{
			MethodName: "ClearAdapterBinding",
			Handler:    _AdaptersService_ClearAdapterBinding_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "nupi/api/v1/config.proto",
}

type QuickstartServiceClient interface {
	GetStatus(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*QuickstartStatusResponse, error)
	Update(ctx context.Context, in *UpdateQuickstartRequest, opts ...grpc.CallOption) (*QuickstartStatusResponse, error)
}

type quickstartServiceClient struct {
	cc grpc.ClientConnInterface
}

func NewQuickstartServiceClient(cc grpc.ClientConnInterface) QuickstartServiceClient {
	return &quickstartServiceClient{cc}
}

func (c *quickstartServiceClient) GetStatus(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*QuickstartStatusResponse, error) {
	out := new(QuickstartStatusResponse)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.QuickstartService/GetStatus", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *quickstartServiceClient) Update(ctx context.Context, in *UpdateQuickstartRequest, opts ...grpc.CallOption) (*QuickstartStatusResponse, error) {
	out := new(QuickstartStatusResponse)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.QuickstartService/Update", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

type QuickstartServiceServer interface {
	GetStatus(context.Context, *emptypb.Empty) (*QuickstartStatusResponse, error)
	Update(context.Context, *UpdateQuickstartRequest) (*QuickstartStatusResponse, error)
	mustEmbedUnimplementedQuickstartServiceServer()
}

type UnimplementedQuickstartServiceServer struct{}

func (UnimplementedQuickstartServiceServer) GetStatus(context.Context, *emptypb.Empty) (*QuickstartStatusResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method GetStatus not implemented")
}
func (UnimplementedQuickstartServiceServer) Update(context.Context, *UpdateQuickstartRequest) (*QuickstartStatusResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method Update not implemented")
}
func (UnimplementedQuickstartServiceServer) mustEmbedUnimplementedQuickstartServiceServer() {}

func RegisterQuickstartServiceServer(s grpc.ServiceRegistrar, srv QuickstartServiceServer) {
	s.RegisterService(&QuickstartService_ServiceDesc, srv)
}

func _QuickstartService_GetStatus_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(emptypb.Empty)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(QuickstartServiceServer).GetStatus(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.QuickstartService/GetStatus",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(QuickstartServiceServer).GetStatus(ctx, req.(*emptypb.Empty))
	}
	return interceptor(ctx, in, info, handler)
}

func _QuickstartService_Update_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(UpdateQuickstartRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(QuickstartServiceServer).Update(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.QuickstartService/Update",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(QuickstartServiceServer).Update(ctx, req.(*UpdateQuickstartRequest))
	}
	return interceptor(ctx, in, info, handler)
}

var QuickstartService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "nupi.api.v1.QuickstartService",
	HandlerType: (*QuickstartServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "GetStatus",
			Handler:    _QuickstartService_GetStatus_Handler,
		},
		{
			MethodName: "Update",
			Handler:    _QuickstartService_Update_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "nupi/api/v1/config.proto",
}
