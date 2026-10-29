package service

import (
	"testing"

	irodsclient_fs "github.com/cyverse/go-irodsclient/fs"
	"github.com/cyverse/irodsfsd/service/api"
)

func TestApplyConnectionConfigTCPBufferSizes(t *testing.T) {
	tests := []struct {
		name     string
		source   *api.ConnectionConfig
		wantSend int
		wantRecv int
	}{
		{
			name: "send and receive sizes are applied independently",
			source: &api.ConnectionConfig{
				TcpSendBufferSize: int32Pointer(8192),
				TcpRecvBufferSize: int32Pointer(16384),
			},
			wantSend: 8192,
			wantRecv: 16384,
		},
		{
			name:     "unset sizes keep defaults",
			source:   &api.ConnectionConfig{},
			wantSend: 0,
			wantRecv: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := irodsclient_fs.ConnectionConfig{}
			if err := applyConnectionConfig(&target, test.source, "metadata_connection"); err != nil {
				t.Fatalf("applyConnectionConfig() error = %v", err)
			}
			if target.TcpSendBufferSize != test.wantSend || target.TcpRecvBufferSize != test.wantRecv {
				t.Fatalf("tcp buffer sizes = (%d, %d), want (%d, %d)", target.TcpSendBufferSize, target.TcpRecvBufferSize, test.wantSend, test.wantRecv)
			}
		})
	}
}
