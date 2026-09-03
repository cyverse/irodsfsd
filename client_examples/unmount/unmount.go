package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cyverse/irodsfsd/client"
	log "github.com/sirupsen/logrus"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "unmount: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	endpoint := flag.String("endpoint", "unix:///var/lib/irodsfsd/comm.sock", "irodsfsd gRPC endpoint")
	timeout := flag.Duration("timeout", 5*time.Minute, "unmount RPC timeout")
	flag.Parse()
	if flag.NArg() != 1 {
		return fmt.Errorf("usage: unmount [options] <mount-id>")
	}

	logger := log.WithField("command", "unmount")
	serviceClient := client.NewMountServiceClient(*endpoint, *timeout, true, logger)
	if err := serviceClient.Connect(); err != nil {
		return err
	}
	defer serviceClient.Disconnect()

	mount, err := serviceClient.Unmount(flag.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("mount_id: %s\nstate: %s\n", mount.GetMountId(), mount.GetState())
	return nil
}
