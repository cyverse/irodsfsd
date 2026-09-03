package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cyverse/irodsfsd/client"
	"github.com/cyverse/irodsfsd/service/api"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protojson"
)

// LoadMountConfigJSONFile reads a protobuf JSON representation of MountConfig.
// Unknown fields are rejected so misspelled mount settings cannot be ignored.
func LoadMountConfigJSONFile(path string) (*client.MountConfig, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("mount config path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read mount config file %q", path)
	}
	config := &api.MountConfig{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data, config); err != nil {
		return nil, errors.Wrapf(err, "failed to parse mount config file %q", path)
	}
	return config, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mount: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	endpoint := flag.String("endpoint", "unix:///var/lib/irodsfsd/comm.sock", "irodsfsd gRPC endpoint")
	timeout := flag.Duration("timeout", 5*time.Minute, "mount RPC timeout")
	mountID := flag.String("mount-id", "", "optional caller-supplied mount ID")
	flag.Parse()
	if flag.NArg() != 1 {
		return fmt.Errorf("usage: mount [options] <mount-config.json>")
	}

	config, err := LoadMountConfigJSONFile(flag.Arg(0))
	if err != nil {
		return err
	}
	logger := log.WithField("command", "mount")
	serviceClient := client.NewMountServiceClient(*endpoint, *timeout, true, logger)
	if err := serviceClient.Connect(); err != nil {
		return err
	}
	defer serviceClient.Disconnect()

	var mount *client.MountInfo
	if *mountID == "" {
		mount, err = serviceClient.Mount(config)
	} else {
		mount, err = serviceClient.MountWithID(*mountID, config)
	}
	if err != nil {
		return err
	}
	fmt.Printf("mount_id: %s\nstate: %s\n", mount.GetMountId(), mount.GetState())
	return nil
}
