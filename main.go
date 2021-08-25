package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"log"
	"os"
	"runtime"
	"strings"
	"time"
	"win-registrator/bridge"
)

func assert(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

var hostIp = flag.String("ip", "", "IP for ports mapped to the host")
var internal = flag.Bool("internal", false, "Use internal ports instead of published ones")
var explicit = flag.Bool("explicit", false, "Only register containers which have SERVICE_NAME label set")
var useIpFromLabel = flag.String("useIpFromLabel", "", "Use IP which is stored in a label assigned to the container")
var refreshInterval = flag.Int("ttl-refresh", 0, "Frequency with which service TTLs are refreshed")
var refreshTtl = flag.Int("ttl", 0, "TTL for services (default is no expiry)")
var forceTags = flag.String("tags", "", "Append tags for all registered services")
var resyncInterval = flag.Int("resync", 0, "Frequency with which services are resynchronized")
var deregister = flag.String("deregister", "always", "Deregister exited services \"always\" or \"on-success\"")
var retryAttempts = flag.Int("retry-attempts", 0, "Max retry attempts to establish a connection with the backend. Use -1 for infinite retries")
var retryInterval = flag.Int("retry-interval", 2000, "Interval (in millisecond) between retry-attempts.")
var cleanup = flag.Bool("cleanup", false, "Remove dangling services")

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s [options] <registry URI>\n\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	if flag.NArg() != 1 {
		if flag.NArg() == 0 {
			fmt.Fprint(os.Stderr, "Missing required argument for registry URI.\n\n")
		} else {
			fmt.Fprintln(os.Stderr, "Extra unparsed arguments:")
			fmt.Fprintln(os.Stderr, " ", strings.Join(flag.Args()[1:], " "))
			fmt.Fprint(os.Stderr, "Options should come before the registry URI argument.\n\n")
		}
		flag.Usage()
		os.Exit(2)
	}

	if *hostIp != "" {
		log.Println("Forcing host IP to", *hostIp)
	}

	if (*refreshTtl == 0 && *refreshInterval > 0) || (*refreshTtl > 0 && *refreshInterval == 0) {
		assert(errors.New("-ttl and -ttl-refresh must be specified together or not at all"))
	} else if *refreshTtl > 0 && *refreshTtl <= *refreshInterval {
		assert(errors.New("-ttl must be greater than -ttl-refresh"))
	}

	if *retryInterval <= 0 {
		assert(errors.New("-retry-interval must be greater than 0"))
	}
	dockerHost := os.Getenv("DOCKER_HOST")
	if dockerHost == "" {
		if runtime.GOOS != "windows" {
			os.Setenv("DOCKER_HOST", "unix:///tmp/docker.sock")
		} else {
			os.Setenv("DOCKER_HOST", "npipe:////./pipe/docker_engine")
		}
	}
	dockerClient, err := client.NewClientWithOpts(client.WithVersion("1.40"), client.FromEnv)
	if err != nil {
		assert(err)
	}

	ctx := context.Background()
	ping, err := dockerClient.Ping(ctx)
	if err != nil {
		assert(err)
	} else {
		log.Println("Docker Engine API Version: " + ping.APIVersion)
	}

	regBridge, err := bridge.New(dockerClient, flag.Arg(0), bridge.Config{
		HostIp:          *hostIp,
		Internal:        *internal,
		Explicit:        *explicit,
		UseIpFromLabel:  *useIpFromLabel,
		ForceTags:       *forceTags,
		RefreshTtl:      *refreshTtl,
		RefreshInterval: *refreshInterval,
		DeregisterCheck: *deregister,
		Cleanup:         *cleanup,
	}, ctx)
	assert(err)

	attempt := 0
	for *retryAttempts == -1 || attempt <= *retryAttempts {
		log.Printf("Connecting to backend (%v/%v)", attempt, *retryAttempts)

		err := regBridge.Ping()
		if err == nil {
			break
		}

		if attempt == *retryAttempts {
			assert(err)
		}
		time.Sleep(time.Duration(*retryInterval) * time.Millisecond)
		attempt++
	}

	regBridge.Sync(false)

	quit := make(chan struct{})

	// Start the TTL refresh timer
	if *refreshInterval > 0 {
		ticker := time.NewTicker(time.Duration(*refreshInterval) * time.Second)
		go func() {
			for {
				select {
				case <-ticker.C:
					regBridge.Refresh()
				case <-quit:
					ticker.Stop()
					return
				}
			}
		}()
	}

	// Start the resync timer if enabled
	if *resyncInterval > 0 {
		resyncTicker := time.NewTicker(time.Duration(*resyncInterval) * time.Second)
		go func() {
			for {
				select {
				case <-resyncTicker.C:
					regBridge.Sync(true)
				case <-quit:
					resyncTicker.Stop()
					return
				}
			}
		}()
	}

	var messages, errs = dockerClient.Events(ctx, types.EventsOptions{})

	// Process Docker events
	for {
		select {
		case err := <-errs:
			assert(err)
		case msg := <-messages:
			switch msg.Status {
			case "start":
				go regBridge.Add(msg.ID)
			case "die":
				go regBridge.RemoveOnExit(msg.ID)
			}
		}
	}

	close(quit)
	log.Fatal("Docker event loop closed")
}
