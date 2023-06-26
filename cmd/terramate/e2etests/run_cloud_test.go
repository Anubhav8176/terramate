// Copyright 2023 Terramate GmbH
// SPDX-License-Identifier: MPL-2.0

package e2etest

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/madlambda/spells/assert"
	"github.com/terramate-io/terramate/cloud"
	"github.com/terramate-io/terramate/cloud/testserver"
	"github.com/terramate-io/terramate/test/sandbox"
)

func TestCLIRunWithCloudSync(t *testing.T) {
	t.Parallel()

	type want struct {
		run    runExpected
		events eventsResponse
	}
	type testcase struct {
		name       string
		layout     []string
		skipIDGen  bool
		cancel     bool
		workingDir string
		cmd        []string
		want       want
	}

	fakeserver := &http.Server{
		Handler: testserver.Router(),
		Addr:    "localhost:3001",
	}

	const fakeserverShutdownTimeout = 3 * time.Second
	errChan := make(chan error)
	go func() {
		errChan <- fakeserver.ListenAndServe()
	}()

	t.Cleanup(func() {
		err := fakeserver.Close()
		if err != nil {
			t.Logf("fakeserver HTTP Close error: %v", err)
		}
		select {
		case err := <-errChan:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Error(err)
			}
		case <-time.After(fakeserverShutdownTimeout):
			t.Error("time excedeed waiting for fakeserver shutdown")
		}
	})

	for _, tc := range []testcase{
		{
			name: "all stacks must have ids",
			layout: []string{
				"s:s1",
				"s:s2",
			},
			skipIDGen: true,
			want: want{
				run: runExpected{
					Status:      1,
					StderrRegex: "flag requires that selected stacks contain an ID field",
				},
			},
		},
		{
			name:   "failed command",
			layout: []string{"s:stack"},
			cmd:    []string{"non-existent-command"},
			want: want{
				run: runExpected{
					Status:      1,
					StderrRegex: "executable file not found",
				},
				events: eventsResponse{
					"stack": []string{"pending", "running", "failed"},
				},
			},
		},
		{
			name:   "canceled command",
			layout: []string{"s:stack"},
			cancel: true,
			want: want{
				run: runExpected{
					Status:       1,
					Stdout:       "ready\ninterrupt\ninterrupt\ninterrupt\n",
					IgnoreStderr: true,
				},
				events: eventsResponse{
					"stack": []string{"pending", "running", "canceled"},
				},
			},
		},
		{
			name:   "basic success sync",
			layout: []string{"s:stack"},
			want: want{
				run: runExpected{
					Status: 0,
					Stdout: "/stack\n",
				},
				events: eventsResponse{
					"stack": []string{"pending", "running", "ok"},
				},
			},
		},
		{
			name: "multiple stacks",
			layout: []string{
				"s:s1",
				"s:s2",
			},
			want: want{
				run: runExpected{
					Status: 0,
					Stdout: "/s1\n/s2\n",
				},
				events: eventsResponse{
					"s1": []string{"pending", "running", "ok"},
					"s2": []string{"pending", "running", "ok"},
				},
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s := sandbox.New(t)
			var genIdsLayout []string
			ids := []string{}
			if !tc.skipIDGen {
				for _, layout := range tc.layout {
					if layout[0] == 's' {
						if strings.Contains(layout, "id=") {
							t.Fatalf("testcases should not contain stack IDs but found %s", layout)
						}
						id := strings.Replace(layout[2:]+"-id-"+t.Name(), "/", "-", -1)
						ids = append(ids, id)
						layout += ":id=" + id
					}
					genIdsLayout = append(genIdsLayout, layout)
				}
			} else {
				genIdsLayout = tc.layout
			}

			s.BuildTree(genIdsLayout)
			s.Git().CommitAll("all stacks committed")
			cli := newCLI(t, filepath.Join(s.RootDir(), tc.workingDir))
			uuid, err := uuid.NewRandom()
			assert.NoError(t, err)
			runid := uuid.String()
			cli.appendEnv = []string{"GITHUB_RUN_ID=" + runid}

			cmd := []string{"run", "--cloud-sync-deployment"}
			if tc.cancel {
				cmd = append(cmd, testHelperBin, "hang")
			} else {
				if len(tc.cmd) > 0 {
					cmd = append(cmd, tc.cmd...)
				} else {
					cmd = append(cmd, testHelperBin, "stack-abs-path", s.RootDir())
				}
			}

			exec := cli.newCmd(cmd...)

			if tc.cancel {
				exec.setpgid()
				exec.start()
				assert.NoError(t, pollBufferForMsgs(exec.stdout, "ready"))
				sendUntilMsgIsReceived(t, exec, os.Interrupt, "ready", "interrupt")
				sendUntilMsgIsReceived(t, exec, os.Interrupt, "ready", "interrupt", "interrupt")

				// We can't check the last interrupt message since the child process
				// may be killed by Terramate with SIGKILL before it gets the signal
				// or it is able to send messages to stdout.
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()

				errs := make(chan error)
				go func() {
					errs <- exec.wait()
					close(errs)
				}()

			outer:
				for ctx.Err() == nil {
					t.Log("sending last interrupt signal to terramate")
					exec.signalGroup(os.Interrupt)

					sendctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()

					select {
					case err := <-errs:
						t.Logf("terramate err: %v", err)
						t.Logf("terramate stdout:\n%s\n", exec.stdout.String())
						t.Logf("terramate stderr:\n%s\n", exec.stderr.String())
						assert.Error(t, err)
						break outer
					case <-sendctx.Done():
						t.Log("terramate still running, re-sending interrupt")
					}
				}

				t.Logf("terramate stdout:\n%s\n", exec.stdout.String())
				t.Logf("terramate stderr:\n%s\n", exec.stderr.String())
			} else {
				exec.start()
				_ = exec.wait()
			}

			result := runResult{
				Cmd:    strings.Join(cmd, " "),
				Stdout: exec.stdout.String(),
				Stderr: exec.stderr.String(),
				Status: exec.exitCode(),
			}
			assertRunResult(t, result, tc.want.run)
			assertRunEvents(t, runid, ids, tc.want.events)
		})
	}
}

func assertRunEvents(t *testing.T, runid string, ids []string, events map[string][]string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	expectedEvents := eventsResponse{}
	if events == nil {
		events = make(map[string][]string)
	}

	for stackpath, ev := range events {
		found := false
		for _, id := range ids {
			if strings.HasPrefix(id, strings.ReplaceAll(stackpath+"-id-", "/", "-")) {
				expectedEvents[id] = ev
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("generated id not found for stack %s", stackpath)
		}
	}

	res, err := cloud.Request[eventsResponse](ctx, &cloud.Client{
		BaseURL:    "http://localhost:3001",
		Credential: &credential{},
	}, "GET", "/v1/deployments/"+testserver.DefaultOrgUUID+"/"+runid+"/events", nil)
	assert.NoError(t, err)

	if diff := cmp.Diff(res, expectedEvents); diff != "" {
		t.Logf("want: %+v", expectedEvents)
		t.Logf("got: %+v", res)
		t.Fatal(diff)
	}
}

type credential struct{}

func (c *credential) Token() (string, error) {
	return "abcd", nil
}

type eventsResponse map[string][]string

func (res eventsResponse) Validate() error {
	return nil
}
