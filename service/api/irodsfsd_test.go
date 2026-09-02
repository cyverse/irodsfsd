package api

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestMountServiceMethods(t *testing.T) {
	service := File_service_api_api_proto.Services().ByName("MountService")
	if service == nil {
		t.Fatal("MountService descriptor is missing")
	}

	want := []string{
		"Mount",
		"Unmount",
		"ListMounts",
		"GetMount",
	}
	got := make([]string, 0, service.Methods().Len())
	for index := 0; index < service.Methods().Len(); index++ {
		got = append(got, string(service.Methods().Get(index).Name()))
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MountService methods = %v, want %v", got, want)
	}
}

func TestMountInfoReusesConfig(t *testing.T) {
	accountFields := (&Account{}).ProtoReflect().Descriptor().Fields()
	for _, name := range []protoreflect.Name{"irods_user_password", "irods_ticket", "irods_pam_token"} {
		if accountFields.ByName(name) == nil {
			t.Errorf("Account.%s is missing", name)
		}
	}

	configField := (&MountInfo{}).ProtoReflect().Descriptor().Fields().ByName("config")
	if configField == nil {
		t.Fatal("MountInfo.config is missing")
	}
	if got, want := configField.Message().FullName(), protoreflect.FullName("api.MountConfig"); got != want {
		t.Errorf("MountInfo.config type = %s, want %s", got, want)
	}
}

func TestMountConfigHasClientConfigOneof(t *testing.T) {
	descriptor := (&MountConfig{}).ProtoReflect().Descriptor()
	clientConfig := descriptor.Oneofs().ByName("client_config")
	if clientConfig == nil {
		t.Fatal("MountConfig.client_config oneof is missing")
	}

	want := []protoreflect.Name{"irodsfs", "davfs", "nfs"}
	got := make([]protoreflect.Name, 0, clientConfig.Fields().Len())
	for index := 0; index < clientConfig.Fields().Len(); index++ {
		got = append(got, clientConfig.Fields().Get(index).Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MountConfig.client_config fields = %v, want %v", got, want)
	}
}

func TestDAVFSAndNFSSchema(t *testing.T) {
	davfsFields := (&DAVFSConfig{}).ProtoReflect().Descriptor().Fields()
	for _, name := range []protoreflect.Name{"url", "username", "password", "config"} {
		if davfsFields.ByName(name) == nil {
			t.Errorf("DAVFSConfig.%s is missing", name)
		}
	}

	port := (&NFSConfig{}).ProtoReflect().Descriptor().Fields().ByName("port")
	if port == nil {
		t.Fatal("NFSConfig.port is missing")
	}
	if port.Kind() != protoreflect.Int32Kind {
		t.Errorf("NFSConfig.port kind = %s, want int32", port.Kind())
	}
}

func TestMountIDIsOptional(t *testing.T) {
	field := (&MountRequest{}).ProtoReflect().Descriptor().Fields().ByName("mount_id")
	if field == nil {
		t.Fatal("MountRequest.mount_id is missing")
	}
	if !field.HasOptionalKeyword() {
		t.Fatal("MountRequest.mount_id must be optional")
	}
}

func TestUnmountRequestHasOnlyMountID(t *testing.T) {
	fields := (&UnmountRequest{}).ProtoReflect().Descriptor().Fields()
	if fields.Len() != 1 || fields.ByName("mount_id") == nil {
		t.Fatalf("UnmountRequest fields = %v, want only mount_id", fields)
	}
}
