package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/takama/daemon"
	"gopkg.in/yaml.v2"
)

const (
	nameFMT     = "harmony-watchdogd@%s"
	description = "Monitor the Harmony blockchain -- `%i`"
	spaceSep    = " "
)

var (
	rootCmd = &cobra.Command{
		Use:          "harmony-watchdogd",
		SilenceUsage: true,
		Long:         "Monitor a Harmony blockchain",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
		},
	}
	w               *cobraSrvWrapper = &cobraSrvWrapper{nil}
	monitorNodeYAML string
	stdlog          *log.Logger
	errlog          *log.Logger
	// Add services here that we might want to depend on, see all services on
	// the machine with systemctl list-unit-files
	dependencies    = []string{}
	errSysIntrpt    = errors.New("daemon was interrupted by system signal")
	errDaemonKilled = errors.New("daemon was killed")
)

// Indirection for cobra
type cobraSrvWrapper struct {
	*Service
}

// Service has embedded daemon
type Service struct {
	daemon.Daemon
	*monitor
	*instruction
}

func (service *Service) monitorNetwork() error {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	// Set up listener for defined host and port
	listener, err := net.Listen(
		"tcp",
		":"+strconv.Itoa(service.instruction.HTTPReporter.Port+1),
	)
	if err != nil {
		return err
	}
	// set up channel on which to send accepted connections
	listen := make(chan net.Conn, 100)
	go service.startReportingHTTPServer(service.instruction)
	go acceptConnection(listener, listen)
	// loop work cycle with accept connections or interrupt
	// by system signal
	killSignal := <-interrupt
	stdlog.Println("[monitorNetwork] Got signal:", killSignal)
	stdlog.Println("[monitorNetwork] Stopping listening on ", listener.Addr())
	listener.Close()
	if killSignal == os.Interrupt {
		return errSysIntrpt
	}
	return errDaemonKilled
}

// Accept a client connection and collect it in a channel
func acceptConnection(listener net.Listener, listen chan<- net.Conn) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		listen <- conn
	}
}

type watchParams struct {
	Auth struct {
		PagerDuty struct {
			EventServiceKey string `yaml:"event-service-key"`
		} `yaml:"pagerduty"`
	} `yaml:"auth"`
	Network struct {
		TargetChain string `yaml:"target-chain"`
		RPCPort     int    `yaml:"public-rpc"`
	} `yaml:"network-config"`
	// Assumes Seconds
	InspectSchedule struct {
		BlockHeader  int `yaml:"block-header"`
		NodeMetadata int `yaml:"node-metadata"`
		CxPending    int `yaml:"cx-pending"`
		CrossLink    int `yaml:"cross-link"`
	} `yaml:"inspect-schedule"`
	Performance struct {
		WorkerPoolSize int `yaml:"num-workers"`
		HTTPTimeout    int `yaml:"http-timeout"`
	} `yaml:"performance"`
	HTTPReporter struct {
		Port int `yaml:"port"`
	} `yaml:"http-reporter"`
	ShardHealthReporting struct {
		Consensus struct {
			Interval int `yaml:"interval"`
			Warning  int `yaml:"warning"`
		} `yaml:"consensus"`
		CxPending struct {
			Warning int `yaml:"pending-limit"`
		} `yaml:"cx-pending"`
		CrossLink struct {
			Warning int `yaml:"warning"`
		} `yaml:"cross-link"`
		ShardHeight struct {
			Warning int `yaml:"tolerance"`
		} `yaml:"shard-height"`
		Connectivity  struct {
			Warning int `yaml:"tolerance"`
		} `yaml:"connectivity"`
	} `yaml:"shard-health-reporting"`
	DistributionFiles struct {
		MachineIPList []string `yaml:"machine-ip-list"`
	} `yaml:"node-distribution"`
}

type committee struct {
	file    string
	members []string
}

type instruction struct {
	watchParams
	superCommittee map[int]committee
}

func newInstructions(yamlPath string) (*instruction, error) {
	rawYAML, err := ioutil.ReadFile(yamlPath)
	if err != nil {
		return nil, err
	}
	t := watchParams{}
	err = yaml.UnmarshalStrict(rawYAML, &t)
	if err != nil {
		return nil, err
	}
	oops := t.sanityCheck()
	if oops != nil {
		return nil, oops
	}
	byShard := make(map[int]committee, len(t.DistributionFiles.MachineIPList))
	for _, file := range t.DistributionFiles.MachineIPList {
		shard := path.Base(strings.TrimSuffix(file, path.Ext(file)))
		id, err := strconv.Atoi(string(shard[len(shard)-1]))
		if err != nil {
			return nil, err
		}
		ipList := []string{}
		f, err := os.Open(file)
		if err != nil {
			return nil, nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			ipList = append(ipList, scanner.Text()+":"+strconv.Itoa(t.Network.RPCPort))
		}
		err = scanner.Err()
		if err != nil {
			return nil, err
		}
		byShard[id] = committee{file, ipList}
	}
	dups := []string{}
	nodeList := make(map[string]string)
	for i, s := range byShard {
		for _, m := range s.members {
			if _, check := nodeList[m]; check {
				dups = append(dups, strconv.FormatInt(int64(i), 10)+": "+m)
				dups = append(dups, nodeList[m]+": "+m)
			} else {
				nodeList[m] = strconv.FormatInt(int64(i), 10)
			}
		}
	}
	if len(nodeList) == 0 {
		return nil, errors.New("empty node list")
	}
	if len(dups) > 0 {
		return nil, errors.New("Duplicate IPs detected.\n" + strings.Join(dups, "\n"))
	}
	return &instruction{t, byShard}, nil
}

func (w *watchParams) sanityCheck() error {
	errList := []string{}
	if w.Network.RPCPort == 0 {
		errList = append(errList, "Missing public-rpc under network-config in yaml config")
	}
	if w.InspectSchedule.BlockHeader == 0 {
		errList = append(errList, "Missing block-header under inspect-schedule in yaml config")
	}
	if w.InspectSchedule.NodeMetadata == 0 {
		errList = append(errList, "Missing node-metadata under inspect-schedule in yaml config")
	}
	if w.InspectSchedule.CxPending == 0 {
		errList = append(errList, "Missing cx-pending under inspect-schedule in yaml config")
	}
	if w.InspectSchedule.CrossLink == 0 {
		errList = append(errList, "Missing cross-link under inspect-schedule in yaml config")
	}
	if w.Performance.WorkerPoolSize == 0 {
		errList = append(errList, "Missing num-workers under performance in yaml config")
	}
	if w.Performance.HTTPTimeout == 0 {
		errList = append(errList, "Missing http-timeout under performance in yaml config")
	}
	if w.HTTPReporter.Port == 0 {
		errList = append(errList, "Missing port under http-reporter in yaml config")
	}
	if w.ShardHealthReporting.Consensus.Interval == 0 {
		errList = append(errList, "Missing warning under shard-health-reporting, interval in yaml config")
	}
	if w.ShardHealthReporting.Consensus.Warning == 0 {
		errList = append(errList, "Missing warning under shard-health-reporting, consensus in yaml config")
	}
	if w.ShardHealthReporting.CxPending.Warning == 0 {
		errList = append(errList, "Missing pending-limit under shard-health-reporting, cx-pending in yaml config")
	}
	if w.ShardHealthReporting.CrossLink.Warning == 0 {
		errList = append(errList, "Missing warning under shard-health-reporting, cross-link in yaml config")
	}
	if w.ShardHealthReporting.ShardHeight.Warning == 0 {
		errList = append(errList, "Missing tolerance under shard-health-reporting, shard-height in yaml config")
	}
	if w.ShardHealthReporting.Connectivity.Warning == 0 {
		errList = append(errList, "Missing tolerance under shard-health-reporting, connectivity in yaml config")
	}
	for _, f := range w.DistributionFiles.MachineIPList {
		_, err := os.Stat(f)
		if os.IsNotExist(err) {
			errList = append(errList, fmt.Sprintf("File not found: %s", f))
		}
	}

	if len(errList) == 0 {
		return nil
	}
	return errors.New(strings.Join(errList, "\n"))
}

func versionS() string {
	return fmt.Sprintf(
		"Harmony (C) 2020. %v, version %v-%v (%v %v)",
		path.Base(os.Args[0]), version, commit, builtBy, builtAt,
	)
}

func init() {
	stdlog = log.New(os.Stdout, "", log.Ldate|log.Ltime)
	errlog = log.New(os.Stderr, "", log.Ldate|log.Ltime)
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Show version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(os.Stderr, versionS()+"\n")
			os.Exit(0)
		},
	})
	rootCmd.AddCommand(serviceCmd())
	rootCmd.AddCommand(monitorCmd())
	rootCmd.AddCommand(generateSampleYAML())
}
