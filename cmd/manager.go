// Copyright © 2016 Genome Research Limited
// Author: Sendu Bala <sb10@sanger.ac.uk>.
//
//  This file is part of VRPipe.
//
//  VRPipe is free software: you can redistribute it and/or modify
//  it under the terms of the GNU Lesser General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  VRPipe is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU Lesser General Public License for more details.
//
//  You should have received a copy of the GNU Lesser General Public License
//  along with VRPipe. If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"fmt"
	"github.com/sb10/vrpipe/jobqueue"
	"github.com/sevlyar/go-daemon"
	"github.com/spf13/cobra"
	"log"
	"os"
	"runtime"
	"syscall"
	"time"
)

// options for this cmd
var foreground bool

// managerCmd represents the manager command
var managerCmd = &cobra.Command{
	Use:   "manager",
	Short: "Pipeline/Job manager",
	Long: `The pipeline management system.

The vrpipe manager works in the background, doing all the work of getting your
pipelines and jobs run successfully.

You'll need to start this running with the 'start' sub-command before you can
achieve anything useful with the other vrpipe commands. If the background
process that is spawned when you run this dies, your pipelines will become
stalled until you run the 'start' sub-command again.`,
}

// start sub-command starts the daemon
var managerStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start pipeline management",
	Long: `Start the pipeline manager, daemonizing it in to the background
(unless --foreground option is supplied).`,
	Run: func(cmd *cobra.Command, args []string) {
		// first we need our working directory to exist
		_, err := os.Stat(config.Manager_dir)
		if err != nil {
			if os.IsNotExist(err) {
				// try and create the directory
				err = os.MkdirAll(config.Manager_dir, os.ModePerm)
				if err != nil {
					log.Fatalf("could not create the working directory '%s': %v\n", config.Manager_dir, err)
				}
			} else {
				log.Fatalf("could not access or create the working directory '%s': %v\n", config.Manager_dir, err)
			}
		}

		// check to see if the manager is already running (regardless of the
		// state of the pid file), giving us a meaningful error message in the
		// most obvious case of failure to start
		jq := connect(10 * time.Millisecond)
		if jq != nil {
			sstats, err := jq.ServerStats()
			var pid int
			if err == nil {
				pid = sstats.ServerInfo.PID
			}
			log.Fatalf("vrpipe manager on port %s is already running (pid %d)\n", config.Manager_port, pid)
		}

		// now daemonize unless in foreground mode
		if foreground {
			syscall.Umask(config.Manager_umask)
			startJQ(true)
		} else {
			context := &daemon.Context{
				PidFileName: config.Manager_pid_file,
				PidFilePerm: 0644,
				WorkDir:     "/",
				Umask:       config.Manager_umask,
			}
			child, err := context.Reborn()
			if err != nil {
				log.Fatalf("failed to daemonize: %s\n", err)
			}
			if child != nil {
				// parent; wait a while for our child to bring up the manager
				// before exiting
				jq := connect(10 * time.Second)
				if jq == nil {
					log.Fatalf("vrpipe manager failed to start on port %s after 10s\n", config.Manager_port)
				}
				sstats, err := jq.ServerStats()
				if err != nil {
					log.Fatalf("vrpipe manager started but doesn't seem to be functional: %s", err)
				}
				logStarted(sstats.ServerInfo)
			} else {
				// daemonized child, that will run until signalled to stop
				defer context.Release()
				startJQ(false)
			}
		}
	},
}

// stop sub-command stops the daemon by sending it a term signal
var managerStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop pipeline management",
	Long:  `Stop the pipeline manager, gracefully shutting down the queues.`,
	Run: func(cmd *cobra.Command, args []string) {
		// the daemon could be running but be non-responsive, or it could have
		// exited but left the pid file in place; to best cover all
		// eventualities we check the pid file first, try and terminate its pid,
		// then confirm we can't connect
		pid, err := daemon.ReadPidFile(config.Manager_pid_file)
		var stopped bool
		if err == nil {
			stopped = stopdaemon(pid, "pid file "+config.Manager_pid_file)
		} else {
			// probably no pid file, we'll see if the daemon is up by trying to
			// connect
			jq := connect(1 * time.Second)
			if jq == nil {
				log.Fatalf("vrpipe manager does not seem to be running on port %s\n", config.Manager_port)
			}
		}

		var jq *jobqueue.Client
		if stopped {
			// we'll do a quick test to confirm the daemon is down
			jq = connect(10 * time.Millisecond)
			if jq != nil {
				log.Printf("according to the pid file %s, vrpipe manager was running with pid %d, and I terminated that pid, but the manager is still up on port %s!\n", config.Manager_pid_file, pid, config.Manager_port)
			} else {
				log.Printf("vrpipe manager running on port %s was gracefully shut down\n", config.Manager_port)
				return
			}
		} else {
			// we failed to SIGTERM the pid in the pid file, let's take some
			// time to confirm the daemon is really up
			jq = connect(5 * time.Second)
			if jq == nil {
				log.Fatalf("according to the pid file %s, vrpipe manager for port %s was running with pid %d, but that process could not be terminated and the manager could not be connected to; most likely the pid file is wrong and the manager is not running - after confirming, delete the pid file before trying to start the manager again\n", config.Manager_pid_file, config.Manager_port, pid)
			}
		}

		// we managed to connect to the daemon; get it's real pid and try to
		// stop it again
		sstats, err := jq.ServerStats()
		if err != nil {
			log.Fatal("even though I was able to connect to the manager, it failed to tell me its true pid; giving up trying to stop it")
		}
		spid := sstats.ServerInfo.PID
		jq.Disconnect()

		stopped = stopdaemon(spid, "the manager itself")
		if stopped {
			log.Printf("vrpipe manager running on port %s was gracefully shut down\n", config.Manager_port)
		} else {
			log.Printf("I've tried everything; giving up trying to stop the manager\n", config.Manager_port)
		}
	},
}

// status sub-command tells if the manger is up or down
// stop sub-command stops the daemon by sending it a term signal
var managerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Get status of the pipeline manager",
	Long:  `Find out if the pipeline manager is currently running or not.`,
	Run: func(cmd *cobra.Command, args []string) {
		// see if pid file suggests it is supposed to be running
		pid, err := daemon.ReadPidFile(config.Manager_pid_file)
		if err == nil {
			// confirm
			jq := connect(5 * time.Second)
			if jq != nil {
				fmt.Println("started")
				return
			}

			log.Fatalf("vrpipe manager on port %s is supposed to be running with pid %d, but is non-responsive\n", config.Manager_port, pid)
		}

		// no pid file, so it's supposed to be down; confirm
		jq := connect(10 * time.Millisecond)
		if jq == nil {
			fmt.Println("stopped")
		} else {
			fmt.Println("started")
		}
	},
}

func init() {
	RootCmd.AddCommand(managerCmd)
	managerCmd.AddCommand(managerStartCmd)
	managerCmd.AddCommand(managerStopCmd)
	managerCmd.AddCommand(managerStatusCmd)

	// flags specific to these sub-commands
	managerStartCmd.Flags().BoolVarP(&foreground, "foreground", "f", false, "Do not daemonize")
}

func connect(wait time.Duration) *jobqueue.Client {
	jq, jqerr := jobqueue.Connect("localhost:"+config.Manager_port, "test_queue", wait)
	if jqerr == nil {
		return jq
	}
	return nil
}

func stopdaemon(pid int, source string) bool {
	err := syscall.Kill(pid, syscall.SIGTERM)
	if err != nil {
		log.Printf("vrpipe manager is running with pid %d according to %s, but failed to send it SIGTERM: %s\n", pid, source, err)
		return false
	}

	// wait a while for the daemon to gracefully close down
	giveupseconds := 15
	giveup := time.After(time.Duration(giveupseconds) * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	stopped := make(chan bool, 1)
	go func() {
		for {
			select {
			case <-ticker.C:
				err = syscall.Kill(pid, syscall.Signal(0))
				if err == nil {
					// pid is still running
					continue
				}
				// assume the error was "no such process" *** should I do a string comparison to confirm?
				ticker.Stop()
				stopped <- true
				return
			case <-giveup:
				ticker.Stop()
				stopped <- false
				return
			}
		}
	}()
	ok := <-stopped

	// if it didn't stop, offer to force kill it? That's a bit dangerous...
	// just warn for now
	if !ok {
		log.Printf("vrpipe manager, running with pid %d according to %s, is still running %ds after I sent it a SIGTERM\n", pid, source, giveupseconds)
	}

	return ok
}

// get a nice address to report in logs, preferring hostname, falling back
// on the ip address if that wasn't set
func sAddr(s *jobqueue.ServerInfo) (addr string) {
	addr = s.Host
	if addr == "localhost" {
		addr = s.Addr
	} else {
		addr += ":" + s.Port
	}
	return
}

func logStarted(s *jobqueue.ServerInfo) {
	log.Printf("vrpipe manager started on %s, pid %d\n", sAddr(s), s.PID)
}

func startJQ(sayStarted bool) {
	runtime.GOMAXPROCS(runtime.NumCPU())

	// start the jobqueue server
	server, err := jobqueue.Serve(config.Manager_port)
	if err != nil {
		log.Fatal(err)
	}

	if sayStarted {
		logStarted(server.ServerInfo)
	}

	// start logging to configured file
	logfile, err := os.OpenFile(config.Manager_log_file, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Printf("could not log to %s, will log to STDOUT: %v\n", config.Manager_log_file, err)
	} else {
		defer logfile.Close()
		log.SetOutput(logfile)
	}

	// log to file that we started
	addr := sAddr(server.ServerInfo)
	log.Printf("vrpipe manager started on %s\n", addr)

	// block forever while the jobqueue does its work
	err = server.Block()
	if err != nil {
		jqerr, ok := err.(jobqueue.Error)
		switch {
		case ok && jqerr.Err == jobqueue.ErrClosedTerm:
			log.Printf("vrpipe manager on %s gracefully stopped (received SIGTERM)\n", addr)
		case ok && jqerr.Err == jobqueue.ErrClosedInt:
			log.Printf("vrpipe manager on %s gracefully stopped (received SIGINT)\n", addr)
		default:
			log.Printf("vrpipe manager on %s exited unexpectedly: %s\n", addr, err)
		}
	}
}
