package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/cyverse/irodsfsd/client"
	log "github.com/sirupsen/logrus"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mount_list: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	endpoint := flag.String("endpoint", "unix:///var/lib/irodsfsd/comm.sock", "irodsfsd gRPC endpoint")
	timeout := flag.Duration("timeout", 30*time.Second, "list RPC timeout")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("usage: mount_list [options]")
	}

	logger := log.WithField("command", "mount_list")
	serviceClient := client.NewMountServiceClient(*endpoint, *timeout, true, logger)
	if err := serviceClient.Connect(); err != nil {
		return err
	}
	defer serviceClient.Disconnect()

	mounts, err := serviceClient.ListMounts(nil)
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "MOUNT_ID\tSTATE\tMOUNT_PATH")
	for _, mount := range mounts {
		fmt.Fprintf(writer, "%s\t%s\t%s\n", mount.GetMountId(), mount.GetState(), mount.GetConfig().GetMountPath())
	}
	return writer.Flush()
}
