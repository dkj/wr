// Copyright © 2016-2017 Genome Research Limited
// Author: Sendu Bala <sb10@sanger.ac.uk>.
//
//  This file is part of wr.
//
//  wr is free software: you can redistribute it and/or modify
//  it under the terms of the GNU Lesser General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  wr is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU Lesser General Public License for more details.
//
//  You should have received a copy of the GNU Lesser General Public License
//  along with wr. If not, see <http://www.gnu.org/licenses/>.

package jobqueue

// This file contains the command line interface code of the server.

import (
	"bytes"
	"fmt"
	"github.com/VertebrateResequencing/wr/queue"
	"github.com/go-mangos/mangos"
	"github.com/satori/go.uuid"
	"github.com/ugorji/go/codec"
	"time"
)

// handleRequest parses the bytes received from a connected client in to a
// clientRequest, does the requested work, then responds back to the client with
// a serverResponse
func (s *Server) handleRequest(m *mangos.Message) error {
	dec := codec.NewDecoderBytes(m.Body, s.ch)
	cr := &clientRequest{}
	err := dec.Decode(cr)
	if err != nil {
		return err
	}

	q := s.getOrCreateQueue(cr.Queue)

	var sr *serverResponse
	var srerr string
	var qerr string

	// check that the client making the request has the expected username; NB:
	// *** this is not real security, since the client could just lie about its
	// username! Right now this is intended to stop accidental use of someone
	// else's jobqueue server
	if cr.User == "" || !s.allowedUsers[cr.User] {
		srerr = ErrWrongUser
		qerr = fmt.Sprintf("User %s denied access (only %s allowed)", cr.User, s.ServerInfo.AllowedUsers)
	} else if q == nil {
		// the server just got shutdown, we shouldn't really end up here?... Can
		// we even respond??
		srerr = ErrClosedStop
		qerr = "The server has been stopped"
	} else {
		switch cr.Method {
		case "ping":
			// do nothing - not returning an error to client means ping success
		case "sstats":
			sr = &serverResponse{SStats: s.GetServerStats()}
		case "backup":
			// make an io.Writer that writes to a byte slice, so we can return
			// the db as that
			var b bytes.Buffer
			err := s.BackupDB(&b)
			if err != nil {
				srerr = ErrInternalError
				qerr = err.Error()
			} else {
				sr = &serverResponse{DB: b.Bytes()}
			}
		case "drain":
			err := s.Drain()
			if err != nil {
				srerr = ErrInternalError
				qerr = err.Error()
			} else {
				sr = &serverResponse{SStats: s.GetServerStats()}
			}
		case "shutdown":
			err := s.Stop()
			if err != nil {
				srerr = ErrInternalError
				qerr = err.Error()
			}
		case "add":
			// add jobs to the queue, and along side keep the environment variables
			// they're supposed to execute under.
			if cr.Env == nil || cr.Jobs == nil {
				srerr = ErrBadRequest
			} else {
				// Store Env
				envkey, err := s.db.storeEnv(cr.Env)
				if err != nil {
					srerr = ErrDBError
					qerr = err.Error()
				} else {
					if srerr == "" {
						// create the jobs server-side
						added, dups, alreadyComplete, thisSrerr, err := s.createJobs(q, cr.Jobs, envkey, cr.IgnoreComplete)
						if err != nil {
							srerr = thisSrerr
							qerr = err.Error()
						} else {
							sr = &serverResponse{Added: added, Existed: dups + alreadyComplete}
						}
					}
				}
			}
		case "reserve":
			// return the next ready job
			if cr.ClientID.String() == "00000000-0000-0000-0000-000000000000" {
				srerr = ErrBadRequest
			} else if !s.drain {
				// first just try to Reserve normally
				var item *queue.Item
				var err error
				if cr.SchedulerGroup != "" {
					// if this is the first job that the client is trying to
					// reserve, and if we don't actually want any more clients
					// working on this schedulerGroup, we'll just act as if nothing
					// was ready. Likewise if in drain mode.
					skip := false
					if cr.FirstReserve && s.rc != "" {
						s.sgcmutex.Lock()
						if count, existed := s.sgroupcounts[cr.SchedulerGroup]; !existed || count == 0 {
							skip = true
						}
						s.sgcmutex.Unlock()
					}

					if !skip {
						item, err = q.Reserve(cr.SchedulerGroup)
					}
				} else {
					item, err = q.Reserve()
				}

				if err != nil {
					if qerr, ok := err.(queue.Error); ok && qerr.Err == queue.ErrNothingReady {
						// there's nothing in the ready sub queue right now, so every
						// second try and Reserve() from the queue until either we get
						// an item, or we exceed the client's timeout
						var stop <-chan time.Time
						if cr.Timeout.Nanoseconds() > 0 {
							stop = time.After(cr.Timeout)
						} else {
							stop = make(chan time.Time)
						}

						itemerrch := make(chan *itemErr, 1)
						ticker := time.NewTicker(ServerReserveTicker)
						go func() {
							for {
								select {
								case <-ticker.C:
									item, err := q.Reserve(cr.SchedulerGroup)
									if err != nil {
										if qerr, ok := err.(queue.Error); ok && qerr.Err == queue.ErrNothingReady {
											continue
										}
										ticker.Stop()
										if qerr, ok := err.(queue.Error); ok && qerr.Err == queue.ErrQueueClosed {
											itemerrch <- &itemErr{err: ErrQueueClosed}
										} else {
											itemerrch <- &itemErr{err: ErrInternalError}
										}
										return
									}
									ticker.Stop()
									itemerrch <- &itemErr{item: item}
									return
								case <-stop:
									ticker.Stop()
									// if we time out, we'll return nil job and nil err
									itemerrch <- &itemErr{}
									return
								}
							}
						}()
						itemerr := <-itemerrch
						close(itemerrch)
						item = itemerr.item
						srerr = itemerr.err
					}
				}
				if srerr == "" && item != nil {
					// clean up any past state to have a fresh job ready to run
					sjob := item.Data.(*Job)
					sjob.Lock()
					sjob.ReservedBy = cr.ClientID //*** we should unset this on moving out of run state, to save space
					sjob.Exited = false
					sjob.Pid = 0
					sjob.Host = ""
					var tnil time.Time
					sjob.StartTime = tnil
					sjob.EndTime = tnil
					sjob.PeakRAM = 0
					sjob.Exitcode = -1
					sjob.Unlock()

					q.SetDelay(item.Key, ClientReleaseDelay)

					// make a copy of the job with some extra stuff filled in (that
					// we don't want taking up memory here) for the client
					job := s.itemToJob(item, false, true)
					sr = &serverResponse{Job: job}
				}
			} // else we'll return nothing, as if there were no jobs in the queue
		case "jstart":
			// update the job's cmd-started-related properties
			var job *Job
			_, job, srerr = s.getij(cr, q)
			if srerr == "" {
				job.Lock()
				if cr.Job.Pid <= 0 || cr.Job.Host == "" {
					srerr = ErrBadRequest
				} else {
					job.Host = cr.Job.Host
					if job.Host != "" {
						job.HostID = s.scheduler.HostToID(job.Host)
					}
					job.HostIP = cr.Job.HostIP
					job.Pid = cr.Job.Pid
					job.StartTime = time.Now()
					var tend time.Time
					job.EndTime = tend
					job.Attempts++
					job.killCalled = false
					job.Lost = false
				}
				job.Unlock()
			}
		case "jtouch":
			var job *Job
			var item *queue.Item
			item, job, srerr = s.getij(cr, q)
			if srerr == "" {
				// if kill has been called for this job, just return KillCalled
				job.Lock()
				killCalled := job.killCalled
				lost := job.Lost
				job.Unlock()

				if !killCalled {
					// also just return killCalled if server has been set to
					// kill all jobs
					s.krmutex.RLock()
					killCalled = s.killRunners
					s.krmutex.RUnlock()
				}

				if !killCalled {
					// else, update the job's ttr
					err = q.Touch(item.Key)
					if err != nil {
						srerr = ErrInternalError
						qerr = err.Error()
					} else if lost {
						job.Lock()
						job.Lost = false
						job.EndTime = time.Time{}
						job.Unlock()

						// since our changed callback won't be called, send out
						// this transition from lost to running state
						s.statusCaster.Send(&jstateCount{"+all+", JobStateLost, JobStateRunning, 1})
						s.statusCaster.Send(&jstateCount{job.RepGroup, JobStateLost, JobStateRunning, 1})
					}
				}
				sr = &serverResponse{KillCalled: killCalled}
			}
		case "jend":
			// update the job's cmd-ended-related properties
			var job *Job
			_, job, srerr = s.getij(cr, q)
			if srerr == "" {
				job.Lock()
				job.Exited = true
				job.Exitcode = cr.Job.Exitcode
				job.PeakRAM = cr.Job.PeakRAM
				job.CPUtime = cr.Job.CPUtime
				job.EndTime = time.Now()
				job.ActualCwd = cr.Job.ActualCwd
				job.Unlock()
				s.db.updateJobAfterExit(job, cr.Job.StdOutC, cr.Job.StdErrC, false)
			}
		case "jarchive":
			// remove the job from the queue, rpl and live bucket and add to
			// complete bucket
			var item *queue.Item
			var job *Job
			item, job, srerr = s.getij(cr, q)
			if srerr == "" {
				// first check the item is still in the run queue (eg. the job
				// wasn't released by another process; unlike the other methods,
				// queue package does not check we're in the run queue when
				// Remove()ing, since you can remove from any queue)
				job.Lock()
				if running := item.Stats().State == queue.ItemStateRun; !running {
					srerr = ErrBadJob
					job.Unlock()
				} else if !job.Exited || job.Exitcode != 0 || job.StartTime.IsZero() || job.EndTime.IsZero() {
					// the job must also have gone through jend
					srerr = ErrBadRequest
					job.Unlock()
				} else {
					key := job.key()
					job.State = JobStateComplete
					job.FailReason = ""
					job.Unlock()
					err := s.db.archiveJob(key, job)
					if err != nil {
						srerr = ErrDBError
						qerr = err.Error()
					} else {
						err = q.Remove(key)
						if err != nil {
							srerr = ErrInternalError
							qerr = err.Error()
						} else {
							s.rpl.Lock()
							if m, exists := s.rpl.lookup[job.RepGroup]; exists {
								delete(m, key)
							}
							s.rpl.Unlock()
							s.decrementGroupCount(job.schedulerGroup, q)
						}
					}
				}
			}
		case "jrelease":
			// move the job from the run queue to the delay queue, unless it has
			// failed too many times, in which case bury
			var item *queue.Item
			var job *Job
			item, job, srerr = s.getij(cr, q)
			if srerr == "" {
				job.Lock()
				job.FailReason = cr.Job.FailReason
				if !job.StartTime.IsZero() {
					// obey jobs's Retries count by adjusting UntilBuried if a
					// client reserved this job and started to run the job's cmd
					job.UntilBuried--
				}
				if job.Exited && job.Exitcode != 0 {
					job.updateRecsAfterFailure()
				}
				if job.UntilBuried <= 0 {
					job.Unlock()
					err = q.Bury(item.Key)
					if err != nil {
						srerr = ErrInternalError
						qerr = err.Error()
					} else {
						s.decrementGroupCount(job.getSchedulerGroup(), q)
					}
				} else {
					job.Unlock()
					err = q.Release(item.Key)
					if err != nil {
						srerr = ErrInternalError
						qerr = err.Error()
					} else {
						s.decrementGroupCount(job.getSchedulerGroup(), q)
					}
				}
			}
		case "jbury":
			// move the job from the run queue to the bury queue
			var item *queue.Item
			var job *Job
			item, job, srerr = s.getij(cr, q)
			if srerr == "" {
				job.Lock()
				job.FailReason = cr.Job.FailReason
				job.Unlock()
				err = q.Bury(item.Key)
				if err != nil {
					srerr = ErrInternalError
					qerr = err.Error()
				} else {
					s.decrementGroupCount(job.getSchedulerGroup(), q)

					if len(cr.Job.StdErrC) > 0 {
						s.db.updateJobAfterExit(job, cr.Job.StdOutC, cr.Job.StdErrC, true)
					}
				}
			}
		case "jkick":
			// move the jobs from the bury queue to the ready queue; unlike the
			// other j* methods, client doesn't have to be the Reserve() owner
			// of these jobs, and we don't want the "in run queue" test
			if cr.Keys == nil {
				srerr = ErrBadRequest
			} else {
				kicked := 0
				for _, jobkey := range cr.Keys {
					item, err := q.Get(jobkey)
					if err != nil || item.Stats().State != queue.ItemStateBury {
						continue
					}
					err = q.Kick(jobkey)
					if err == nil {
						job := item.Data.(*Job)
						job.Lock()
						job.UntilBuried = job.Retries + 1
						job.Unlock()
						kicked++
					}
				}
				sr = &serverResponse{Existed: kicked}
			}
		case "jdel":
			// remove the jobs from the bury queue and the live bucket
			if cr.Keys == nil {
				srerr = ErrBadRequest
			} else {
				deleted := 0
				for _, jobkey := range cr.Keys {
					item, err := q.Get(jobkey)
					if err != nil || item.Stats().State != queue.ItemStateBury {
						continue
					}

					// we can't allow the removal of jobs that have dependencies, as
					// *queue would regard that as satisfying the dependency and
					// downstream jobs would start
					hasDeps, err := q.HasDependents(jobkey)
					if err != nil || hasDeps {
						continue
					}

					err = q.Remove(jobkey)
					if err == nil {
						deleted++
						s.db.deleteLiveJob(jobkey) //*** probably want to batch this up to delete many at once
					}
				}
				sr = &serverResponse{Existed: deleted}
			}
		case "jkill":
			// set the killCalled property on the jobs, to change the subsequent
			// behaviour of jtouch; as per jkick, client doesn't have to be the
			// Reserve() owner of these jobs, though we do want the "in run
			// queue" test
			if cr.Keys == nil {
				srerr = ErrBadRequest
			} else {
				killable := 0
				for _, jobkey := range cr.Keys {
					k, err := s.killJob(q, jobkey)
					if err != nil {
						continue
					}
					if k {
						killable++
					}
				}
				sr = &serverResponse{Existed: killable}
			}
		case "getbc":
			// get jobs by their keys (which come from their Cmds & Cwds)
			if cr.Keys == nil {
				srerr = ErrBadRequest
			} else {
				var jobs []*Job
				jobs, srerr, qerr = s.getJobsByKeys(q, cr.Keys, cr.GetStd, cr.GetEnv)
				if len(jobs) > 0 {
					sr = &serverResponse{Jobs: jobs}
				}
			}
		case "getbr":
			// get jobs by their RepGroup
			if cr.Job == nil || cr.Job.RepGroup == "" {
				srerr = ErrBadRequest
			} else {
				var jobs []*Job
				jobs, srerr, qerr = s.getJobsByRepGroup(q, cr.Job.RepGroup, cr.Limit, cr.State, cr.GetStd, cr.GetEnv)
				if len(jobs) > 0 {
					sr = &serverResponse{Jobs: jobs}
				}
			}
		case "getin":
			// get all jobs in the jobqueue
			jobs := s.getJobsCurrent(q, cr.Limit, cr.State, cr.GetStd, cr.GetEnv)
			if len(jobs) > 0 {
				sr = &serverResponse{Jobs: jobs}
			}
		default:
			srerr = ErrUnknownCommand
		}
	}

	// on error, just send the error back to client and return a more detailed
	// error for logging
	if srerr != "" {
		s.reply(m, &serverResponse{Err: srerr})
		if qerr == "" {
			qerr = srerr
		}
		key := ""
		if cr.Job != nil {
			key = cr.Job.key()
		}
		return Error{cr.Queue, cr.Method, key, qerr}
	}

	// some commands don't return anything to the client
	if sr == nil {
		sr = &serverResponse{}
	}

	// send reply to client
	err = s.reply(m, sr)
	if err != nil {
		// log failure to reply
		return err
	}
	return nil
}

// for the many j* methods in handleRequest, we do this common stuff to get
// the desired item and job.
func (s *Server) getij(cr *clientRequest, q *queue.Queue) (item *queue.Item, job *Job, errs string) {
	// clientRequest must have a Job
	if cr.Job == nil {
		errs = ErrBadRequest
		return
	}

	item, err := q.Get(cr.Job.key())
	if err != nil || item.Stats().State != queue.ItemStateRun {
		errs = ErrBadJob
		return
	}
	job = item.Data.(*Job)

	if !uuid.Equal(cr.ClientID, job.ReservedBy) {
		errs = ErrMustReserve
	}

	return
}

// for the many get* methods in handleRequest, we do this common stuff to get
// an item's job from the in-memory queue formulated for the client.
func (s *Server) itemToJob(item *queue.Item, getStd bool, getEnv bool) (job *Job) {
	sjob := item.Data.(*Job)
	sjob.RLock()

	stats := item.Stats()

	state := itemsStateToJobState[stats.State]
	if state == "" {
		state = JobStateUnknown
	} else if state == JobStateReserved && sjob.Lost {
		state = JobStateLost
	}

	// we're going to fill in some properties of the Job and return
	// it to client, but don't want those properties set here for
	// us, so we make a new Job and fill stuff in that
	job = &Job{
		RepGroup:     sjob.RepGroup,
		ReqGroup:     sjob.ReqGroup,
		DepGroups:    sjob.DepGroups,
		Cmd:          sjob.Cmd,
		Cwd:          sjob.Cwd,
		CwdMatters:   sjob.CwdMatters,
		ChangeHome:   sjob.ChangeHome,
		ActualCwd:    sjob.ActualCwd,
		Requirements: sjob.Requirements,
		Priority:     sjob.Priority,
		Retries:      sjob.Retries,
		PeakRAM:      sjob.PeakRAM,
		Exited:       sjob.Exited,
		Exitcode:     sjob.Exitcode,
		FailReason:   sjob.FailReason,
		StartTime:    sjob.StartTime,
		EndTime:      sjob.EndTime,
		Pid:          sjob.Pid,
		Host:         sjob.Host,
		HostID:       sjob.HostID,
		HostIP:       sjob.HostIP,
		CPUtime:      sjob.CPUtime,
		State:        state,
		Attempts:     sjob.Attempts,
		UntilBuried:  sjob.UntilBuried,
		ReservedBy:   sjob.ReservedBy,
		EnvKey:       sjob.EnvKey,
		EnvOverride:  sjob.EnvOverride,
		Dependencies: sjob.Dependencies,
		Behaviours:   sjob.Behaviours,
		MountConfigs: sjob.MountConfigs,
	}

	if !sjob.StartTime.IsZero() && state == JobStateReserved {
		job.State = JobStateRunning
	}
	sjob.RUnlock()
	s.jobPopulateStdEnv(job, getStd, getEnv)
	return
}

// jobPopulateStdEnv fills in the StdOutC, StdErrC and EnvC values for a Job,
// extracting them from the database.
func (s *Server) jobPopulateStdEnv(job *Job, getStd bool, getEnv bool) {
	job.Lock()
	defer job.Unlock()
	if getStd && ((job.Exited && job.Exitcode != 0) || job.State == JobStateBuried) {
		job.StdOutC, job.StdErrC = s.db.retrieveJobStd(job.key())
	}
	if getEnv {
		job.EnvC = s.db.retrieveEnv(job.EnvKey)
	}
}

// reply to a client
func (s *Server) reply(m *mangos.Message, sr *serverResponse) (err error) {
	var encoded []byte
	enc := codec.NewEncoderBytes(&encoded, s.ch)
	s.racmutex.RLock()
	err = enc.Encode(sr)
	s.racmutex.RUnlock()
	if err != nil {
		return
	}
	m.Body = encoded
	err = s.sock.SendMsg(m)
	return
}
