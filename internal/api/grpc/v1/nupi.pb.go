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
	"google.golang.org/protobuf/types/descriptorpb"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
)

const (
	// Verify that this generated file is compatible with the proto package it is being compiled against.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that this generated file is compatible with the protoimpl package it is being compiled against.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type DaemonStatusRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields
}

func (x *DaemonStatusRequest) Reset() {
	*x = DaemonStatusRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_v1_proto_msgTypes[0]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *DaemonStatusRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*DaemonStatusRequest) ProtoMessage() {}

func (x *DaemonStatusRequest) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_v1_proto_msgTypes[0]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

type DaemonStatusResponse struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Version      string  `protobuf:"bytes,1,opt,name=version,proto3" json:"version,omitempty"`
	Sessions     int32   `protobuf:"varint,2,opt,name=sessions,proto3" json:"sessions,omitempty"`
	Port         int32   `protobuf:"varint,3,opt,name=port,proto3" json:"port,omitempty"`
	GrpcPort     int32   `protobuf:"varint,4,opt,name=grpc_port,json=grpcPort,proto3" json:"grpc_port,omitempty"`
	Binding      string  `protobuf:"bytes,5,opt,name=binding,proto3" json:"binding,omitempty"`
	GrpcBinding  string  `protobuf:"bytes,6,opt,name=grpc_binding,json=grpcBinding,proto3" json:"grpc_binding,omitempty"`
	AuthRequired bool    `protobuf:"varint,7,opt,name=auth_required,json=authRequired,proto3" json:"auth_required,omitempty"`
	UptimeSec    float64 `protobuf:"fixed64,8,opt,name=uptime_sec,json=uptimeSec,proto3" json:"uptime_sec,omitempty"`
}

func (x *DaemonStatusResponse) Reset() {
	*x = DaemonStatusResponse{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_v1_proto_msgTypes[1]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *DaemonStatusResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*DaemonStatusResponse) ProtoMessage() {}

func (x *DaemonStatusResponse) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_v1_proto_msgTypes[1]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *DaemonStatusResponse) GetVersion() string {
	if x != nil {
		return x.Version
	}
	return ""
}

func (x *DaemonStatusResponse) GetSessions() int32 {
	if x != nil {
		return x.Sessions
	}
	return 0
}

func (x *DaemonStatusResponse) GetPort() int32 {
	if x != nil {
		return x.Port
	}
	return 0
}

func (x *DaemonStatusResponse) GetGrpcPort() int32 {
	if x != nil {
		return x.GrpcPort
	}
	return 0
}

func (x *DaemonStatusResponse) GetBinding() string {
	if x != nil {
		return x.Binding
	}
	return ""
}

func (x *DaemonStatusResponse) GetGrpcBinding() string {
	if x != nil {
		return x.GrpcBinding
	}
	return ""
}

func (x *DaemonStatusResponse) GetAuthRequired() bool {
	if x != nil {
		return x.AuthRequired
	}
	return false
}

func (x *DaemonStatusResponse) GetUptimeSec() float64 {
	if x != nil {
		return x.UptimeSec
	}
	return 0
}

type Session struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Id        string   `protobuf:"bytes,1,opt,name=id,proto3" json:"id,omitempty"`
	Command   string   `protobuf:"bytes,2,opt,name=command,proto3" json:"command,omitempty"`
	Args      []string `protobuf:"bytes,3,rep,name=args,proto3" json:"args,omitempty"`
	Status    string   `protobuf:"bytes,4,opt,name=status,proto3" json:"status,omitempty"`
	Pid       int32    `protobuf:"varint,5,opt,name=pid,proto3" json:"pid,omitempty"`
	StartUnix int64    `protobuf:"varint,6,opt,name=start_unix,json=startUnix,proto3" json:"start_unix,omitempty"`
}

func (x *Session) Reset() {
	*x = Session{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_v1_proto_msgTypes[2]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *Session) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Session) ProtoMessage() {}

func (x *Session) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_v1_proto_msgTypes[2]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *Session) GetId() string {
	if x != nil {
		return x.Id
	}
	return ""
}

func (x *Session) GetCommand() string {
	if x != nil {
		return x.Command
	}
	return ""
}

func (x *Session) GetArgs() []string {
	if x != nil {
		return x.Args
	}
	return nil
}

func (x *Session) GetStatus() string {
	if x != nil {
		return x.Status
	}
	return ""
}

func (x *Session) GetPid() int32 {
	if x != nil {
		return x.Pid
	}
	return 0
}

func (x *Session) GetStartUnix() int64 {
	if x != nil {
		return x.StartUnix
	}
	return 0
}

type ListSessionsRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields
}

func (x *ListSessionsRequest) Reset() {
	*x = ListSessionsRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_v1_proto_msgTypes[3]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *ListSessionsRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ListSessionsRequest) ProtoMessage() {}

func (x *ListSessionsRequest) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_v1_proto_msgTypes[3]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

type ListSessionsResponse struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Sessions []*Session `protobuf:"bytes,1,rep,name=sessions,proto3" json:"sessions,omitempty"`
}

func (x *ListSessionsResponse) Reset() {
	*x = ListSessionsResponse{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_v1_proto_msgTypes[4]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *ListSessionsResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ListSessionsResponse) ProtoMessage() {}

func (x *ListSessionsResponse) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_v1_proto_msgTypes[4]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *ListSessionsResponse) GetSessions() []*Session {
	if x != nil {
		return x.Sessions
	}
	return nil
}

type CreateSessionRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Command    string   `protobuf:"bytes,1,opt,name=command,proto3" json:"command,omitempty"`
	Args       []string `protobuf:"bytes,2,rep,name=args,proto3" json:"args,omitempty"`
	WorkingDir string   `protobuf:"bytes,3,opt,name=working_dir,json=workingDir,proto3" json:"working_dir,omitempty"`
	Env        []string `protobuf:"bytes,4,rep,name=env,proto3" json:"env,omitempty"`
	Rows       uint32   `protobuf:"varint,5,opt,name=rows,proto3" json:"rows,omitempty"`
	Cols       uint32   `protobuf:"varint,6,opt,name=cols,proto3" json:"cols,omitempty"`
	Detached   bool     `protobuf:"varint,7,opt,name=detached,proto3" json:"detached,omitempty"`
	Inspect    bool     `protobuf:"varint,8,opt,name=inspect,proto3" json:"inspect,omitempty"`
}

func (x *CreateSessionRequest) Reset() {
	*x = CreateSessionRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_v1_proto_msgTypes[5]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *CreateSessionRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*CreateSessionRequest) ProtoMessage() {}

func (x *CreateSessionRequest) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_v1_proto_msgTypes[5]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *CreateSessionRequest) GetCommand() string {
	if x != nil {
		return x.Command
	}
	return ""
}

func (x *CreateSessionRequest) GetArgs() []string {
	if x != nil {
		return x.Args
	}
	return nil
}

func (x *CreateSessionRequest) GetWorkingDir() string {
	if x != nil {
		return x.WorkingDir
	}
	return ""
}

func (x *CreateSessionRequest) GetEnv() []string {
	if x != nil {
		return x.Env
	}
	return nil
}

func (x *CreateSessionRequest) GetRows() uint32 {
	if x != nil {
		return x.Rows
	}
	return 0
}

func (x *CreateSessionRequest) GetCols() uint32 {
	if x != nil {
		return x.Cols
	}
	return 0
}

func (x *CreateSessionRequest) GetDetached() bool {
	if x != nil {
		return x.Detached
	}
	return false
}

func (x *CreateSessionRequest) GetInspect() bool {
	if x != nil {
		return x.Inspect
	}
	return false
}

type CreateSessionResponse struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Session *Session `protobuf:"bytes,1,opt,name=session,proto3" json:"session,omitempty"`
}

func (x *CreateSessionResponse) Reset() {
	*x = CreateSessionResponse{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_v1_proto_msgTypes[6]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *CreateSessionResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*CreateSessionResponse) ProtoMessage() {}

func (x *CreateSessionResponse) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_v1_proto_msgTypes[6]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *CreateSessionResponse) GetSession() *Session {
	if x != nil {
		return x.Session
	}
	return nil
}

type KillSessionRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	SessionId string `protobuf:"bytes,1,opt,name=session_id,json=sessionId,proto3" json:"session_id,omitempty"`
}

func (x *KillSessionRequest) Reset() {
	*x = KillSessionRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_v1_proto_msgTypes[7]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *KillSessionRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*KillSessionRequest) ProtoMessage() {}

func (x *KillSessionRequest) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_v1_proto_msgTypes[7]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *KillSessionRequest) GetSessionId() string {
	if x != nil {
		return x.SessionId
	}
	return ""
}

type KillSessionResponse struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields
}

func (x *KillSessionResponse) Reset() {
	*x = KillSessionResponse{}
	if protoimpl.UnsafeEnabled {
		mi := &file_nupi_api_v1_proto_msgTypes[8]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *KillSessionResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*KillSessionResponse) ProtoMessage() {}

func (x *KillSessionResponse) ProtoReflect() protoreflect.Message {
	mi := &file_nupi_api_v1_proto_msgTypes[8]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

var File_nupi_api_v1_proto protoreflect.FileDescriptor

var (
	file_nupi_api_v1_proto_once    sync.Once
	file_nupi_api_v1_proto_rawDesc []byte
)

func file_nupi_api_v1_proto_rawDescGZIP() []byte {
	file_nupi_api_v1_proto_once.Do(func() {
		file_nupi_api_v1_proto_rawDesc = protoimpl.X.CompressGZIP(file_nupi_api_v1_proto_rawDesc)
	})
	return file_nupi_api_v1_proto_rawDesc
}

var file_nupi_api_v1_proto_msgTypes = make([]protoimpl.MessageInfo, 9)
var file_nupi_api_v1_proto_goTypes = []interface{}{
	(*DaemonStatusRequest)(nil),   // 0: nupi.api.v1.DaemonStatusRequest
	(*DaemonStatusResponse)(nil),  // 1: nupi.api.v1.DaemonStatusResponse
	(*Session)(nil),               // 2: nupi.api.v1.Session
	(*ListSessionsRequest)(nil),   // 3: nupi.api.v1.ListSessionsRequest
	(*ListSessionsResponse)(nil),  // 4: nupi.api.v1.ListSessionsResponse
	(*CreateSessionRequest)(nil),  // 5: nupi.api.v1.CreateSessionRequest
	(*CreateSessionResponse)(nil), // 6: nupi.api.v1.CreateSessionResponse
	(*KillSessionRequest)(nil),    // 7: nupi.api.v1.KillSessionRequest
	(*KillSessionResponse)(nil),   // 8: nupi.api.v1.KillSessionResponse
	(*emptypb.Empty)(nil),         // 9: google.protobuf.Empty (unused placeholder)
}
var file_nupi_api_v1_proto_depIdxs = []int32{
	2, // 0: nupi.api.v1.ListSessionsResponse.sessions:type_name -> nupi.api.v1.Session
	2, // 1: nupi.api.v1.CreateSessionResponse.session:type_name -> nupi.api.v1.Session
	0, // 2: nupi.api.v1.DaemonService.Status:input_type -> nupi.api.v1.DaemonStatusRequest
	1, // 3: nupi.api.v1.DaemonService.Status:output_type -> nupi.api.v1.DaemonStatusResponse
	3, // 4: nupi.api.v1.SessionsService.ListSessions:input_type -> nupi.api.v1.ListSessionsRequest
	4, // 5: nupi.api.v1.SessionsService.ListSessions:output_type -> nupi.api.v1.ListSessionsResponse
	5, // 6: nupi.api.v1.SessionsService.CreateSession:input_type -> nupi.api.v1.CreateSessionRequest
	6, // 7: nupi.api.v1.SessionsService.CreateSession:output_type -> nupi.api.v1.CreateSessionResponse
	7, // 8: nupi.api.v1.SessionsService.KillSession:input_type -> nupi.api.v1.KillSessionRequest
	8, // 9: nupi.api.v1.SessionsService.KillSession:output_type -> nupi.api.v1.KillSessionResponse
}

func init() {
	file_nupi_api_v1_proto_init()
}

func file_nupi_api_v1_proto_init() {
	if File_nupi_api_v1_proto != nil {
		return
	}

	if !protoimpl.UnsafeEnabled {
		file_nupi_api_v1_proto_msgTypes[0].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*DaemonStatusRequest); i {
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
		file_nupi_api_v1_proto_msgTypes[1].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*DaemonStatusResponse); i {
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
		file_nupi_api_v1_proto_msgTypes[2].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*Session); i {
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
		file_nupi_api_v1_proto_msgTypes[3].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*ListSessionsRequest); i {
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
		file_nupi_api_v1_proto_msgTypes[4].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*ListSessionsResponse); i {
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
		file_nupi_api_v1_proto_msgTypes[5].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*CreateSessionRequest); i {
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
		file_nupi_api_v1_proto_msgTypes[6].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*CreateSessionResponse); i {
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
		file_nupi_api_v1_proto_msgTypes[7].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*KillSessionRequest); i {
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
		file_nupi_api_v1_proto_msgTypes[8].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*KillSessionResponse); i {
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

	fd := &descriptorpb.FileDescriptorProto{
		Syntax:  proto.String("proto3"),
		Name:    proto.String("nupi/api/v1/nupi.proto"),
		Package: proto.String("nupi.api.v1"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("DaemonStatusRequest")},
			{
				Name: proto.String("DaemonStatusResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("version"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("sessions"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()},
					{Name: proto.String("port"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()},
					{Name: proto.String("grpc_port"), Number: proto.Int32(4), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()},
					{Name: proto.String("binding"), Number: proto.Int32(5), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("grpc_binding"), Number: proto.Int32(6), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("auth_required"), Number: proto.Int32(7), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()},
					{Name: proto.String("uptime_sec"), Number: proto.Int32(8), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_DOUBLE.Enum()},
				},
			},
			{
				Name: proto.String("Session"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("id"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("command"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("args"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("status"), Number: proto.Int32(4), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("pid"), Number: proto.Int32(5), Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()},
					{Name: proto.String("start_unix"), Number: proto.Int32(6), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
				},
			},
			{Name: proto.String("ListSessionsRequest")},
			{
				Name: proto.String("ListSessionsResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("sessions"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".nupi.api.v1.Session"),
					},
				},
			},
			{
				Name: proto.String("CreateSessionRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("command"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("args"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("working_dir"), Number: proto.Int32(3), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("env"), Number: proto.Int32(4), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("rows"), Number: proto.Int32(5), Type: descriptorpb.FieldDescriptorProto_TYPE_UINT32.Enum()},
					{Name: proto.String("cols"), Number: proto.Int32(6), Type: descriptorpb.FieldDescriptorProto_TYPE_UINT32.Enum()},
					{Name: proto.String("detached"), Number: proto.Int32(7), Type: descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()},
					{Name: proto.String("inspect"), Number: proto.Int32(8), Type: descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()},
				},
			},
			{
				Name: proto.String("CreateSessionResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("session"),
						Number:   proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".nupi.api.v1.Session"),
					},
				},
			},
			{
				Name: proto.String("KillSessionRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("session_id"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				},
			},
			{Name: proto.String("KillSessionResponse")},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name: proto.String("DaemonService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{
						Name:       proto.String("Status"),
						InputType:  proto.String(".nupi.api.v1.DaemonStatusRequest"),
						OutputType: proto.String(".nupi.api.v1.DaemonStatusResponse"),
					},
				},
			},
			{
				Name: proto.String("SessionsService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{
						Name:       proto.String("ListSessions"),
						InputType:  proto.String(".nupi.api.v1.ListSessionsRequest"),
						OutputType: proto.String(".nupi.api.v1.ListSessionsResponse"),
					},
					{
						Name:       proto.String("CreateSession"),
						InputType:  proto.String(".nupi.api.v1.CreateSessionRequest"),
						OutputType: proto.String(".nupi.api.v1.CreateSessionResponse"),
					},
					{
						Name:       proto.String("KillSession"),
						InputType:  proto.String(".nupi.api.v1.KillSessionRequest"),
						OutputType: proto.String(".nupi.api.v1.KillSessionResponse"),
					},
				},
			},
		},
	}

	rawDesc, err := proto.Marshal(fd)
	if err != nil {
		panic(err)
	}
	file_nupi_api_v1_proto_rawDesc = rawDesc

	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: rawDesc,
			NumEnums:      0,
			NumMessages:   9,
			NumExtensions: 0,
			NumServices:   2,
		},
		GoTypes:           file_nupi_api_v1_proto_goTypes,
		DependencyIndexes: file_nupi_api_v1_proto_depIdxs,
		MessageInfos:      file_nupi_api_v1_proto_msgTypes,
	}.Build()

	File_nupi_api_v1_proto = out.File
	file_nupi_api_v1_proto_rawDesc = nil
}

// gRPC client and server interfaces.

type DaemonServiceClient interface {
	Status(ctx context.Context, in *DaemonStatusRequest, opts ...grpc.CallOption) (*DaemonStatusResponse, error)
}

type daemonServiceClient struct {
	cc grpc.ClientConnInterface
}

func NewDaemonServiceClient(cc grpc.ClientConnInterface) DaemonServiceClient {
	return &daemonServiceClient{cc}
}

func (c *daemonServiceClient) Status(ctx context.Context, in *DaemonStatusRequest, opts ...grpc.CallOption) (*DaemonStatusResponse, error) {
	out := new(DaemonStatusResponse)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.DaemonService/Status", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

type DaemonServiceServer interface {
	Status(context.Context, *DaemonStatusRequest) (*DaemonStatusResponse, error)
	mustEmbedUnimplementedDaemonServiceServer()
}

type UnimplementedDaemonServiceServer struct{}

func (UnimplementedDaemonServiceServer) Status(context.Context, *DaemonStatusRequest) (*DaemonStatusResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method Status not implemented")
}
func (UnimplementedDaemonServiceServer) mustEmbedUnimplementedDaemonServiceServer() {}

func RegisterDaemonServiceServer(s grpc.ServiceRegistrar, srv DaemonServiceServer) {
	s.RegisterService(&DaemonService_ServiceDesc, srv)
}

func _DaemonService_Status_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(DaemonStatusRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(DaemonServiceServer).Status(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.DaemonService/Status",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(DaemonServiceServer).Status(ctx, req.(*DaemonStatusRequest))
	}
	return interceptor(ctx, in, info, handler)
}

var DaemonService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "nupi.api.v1.DaemonService",
	HandlerType: (*DaemonServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "Status",
			Handler:    _DaemonService_Status_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "nupi/api/v1/nupi.proto",
}

type SessionsServiceClient interface {
	ListSessions(ctx context.Context, in *ListSessionsRequest, opts ...grpc.CallOption) (*ListSessionsResponse, error)
	CreateSession(ctx context.Context, in *CreateSessionRequest, opts ...grpc.CallOption) (*CreateSessionResponse, error)
	KillSession(ctx context.Context, in *KillSessionRequest, opts ...grpc.CallOption) (*KillSessionResponse, error)
}

type sessionsServiceClient struct {
	cc grpc.ClientConnInterface
}

func NewSessionsServiceClient(cc grpc.ClientConnInterface) SessionsServiceClient {
	return &sessionsServiceClient{cc}
}

func (c *sessionsServiceClient) ListSessions(ctx context.Context, in *ListSessionsRequest, opts ...grpc.CallOption) (*ListSessionsResponse, error) {
	out := new(ListSessionsResponse)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.SessionsService/ListSessions", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *sessionsServiceClient) CreateSession(ctx context.Context, in *CreateSessionRequest, opts ...grpc.CallOption) (*CreateSessionResponse, error) {
	out := new(CreateSessionResponse)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.SessionsService/CreateSession", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *sessionsServiceClient) KillSession(ctx context.Context, in *KillSessionRequest, opts ...grpc.CallOption) (*KillSessionResponse, error) {
	out := new(KillSessionResponse)
	err := c.cc.Invoke(ctx, "/nupi.api.v1.SessionsService/KillSession", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

type SessionsServiceServer interface {
	ListSessions(context.Context, *ListSessionsRequest) (*ListSessionsResponse, error)
	CreateSession(context.Context, *CreateSessionRequest) (*CreateSessionResponse, error)
	KillSession(context.Context, *KillSessionRequest) (*KillSessionResponse, error)
	mustEmbedUnimplementedSessionsServiceServer()
}

type UnimplementedSessionsServiceServer struct{}

func (UnimplementedSessionsServiceServer) ListSessions(context.Context, *ListSessionsRequest) (*ListSessionsResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ListSessions not implemented")
}
func (UnimplementedSessionsServiceServer) CreateSession(context.Context, *CreateSessionRequest) (*CreateSessionResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method CreateSession not implemented")
}
func (UnimplementedSessionsServiceServer) KillSession(context.Context, *KillSessionRequest) (*KillSessionResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method KillSession not implemented")
}
func (UnimplementedSessionsServiceServer) mustEmbedUnimplementedSessionsServiceServer() {}

func RegisterSessionsServiceServer(s grpc.ServiceRegistrar, srv SessionsServiceServer) {
	s.RegisterService(&SessionsService_ServiceDesc, srv)
}

func _SessionsService_ListSessions_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ListSessionsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(SessionsServiceServer).ListSessions(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.SessionsService/ListSessions",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(SessionsServiceServer).ListSessions(ctx, req.(*ListSessionsRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _SessionsService_CreateSession_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(CreateSessionRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(SessionsServiceServer).CreateSession(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.SessionsService/CreateSession",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(SessionsServiceServer).CreateSession(ctx, req.(*CreateSessionRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _SessionsService_KillSession_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(KillSessionRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(SessionsServiceServer).KillSession(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/nupi.api.v1.SessionsService/KillSession",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(SessionsServiceServer).KillSession(ctx, req.(*KillSessionRequest))
	}
	return interceptor(ctx, in, info, handler)
}

var SessionsService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "nupi.api.v1.SessionsService",
	HandlerType: (*SessionsServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "ListSessions",
			Handler:    _SessionsService_ListSessions_Handler,
		},
		{
			MethodName: "CreateSession",
			Handler:    _SessionsService_CreateSession_Handler,
		},
		{
			MethodName: "KillSession",
			Handler:    _SessionsService_KillSession_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "nupi/api/v1/nupi.proto",
}
