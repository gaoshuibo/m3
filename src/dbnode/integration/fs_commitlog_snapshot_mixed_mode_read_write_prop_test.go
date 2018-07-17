// +build integration

// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package integration

import (
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/m3db/m3db/src/dbnode/integration/generate"
	"github.com/m3db/m3db/src/dbnode/retention"
	"github.com/m3db/m3db/src/dbnode/storage/namespace"
	"github.com/m3db/m3x/context"
	"github.com/m3db/m3x/ident"
	xtime "github.com/m3db/m3x/time"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

const maxBlockSize = 12 * time.Hour
const maxPoints = 1000
const minSuccessfulTests = 8

// This integration test uses property testing to make sure that the node
// can properly bootstrap all the data from a combination of fileset files,
// snapshotfiles, and commit log files. It varies the following inputs to
// the system:
// 		1) block size
// 		2) buffer past
// 		3) buffer future
// 		4) number of datapoints
// 		5) whether it waits for data files to be flushed before shutting down
// 		6) whether it waits for snapshot files to be written before shutting down
//
// It works by generating random datapoints, and then writing those data points
// to the node in order. At randomly selected times during the write process, the
// node will turn itself off and then bootstrap itself before resuming.
func TestFsCommitLogMixedModeReadWriteProp(t *testing.T) {
	if testing.Short() {
		t.SkipNow() // Just skip if we're doing a short run
	}

	var (
		parameters = gopter.DefaultTestParameters()
		seed       = time.Now().UnixNano()
		props      = gopter.NewProperties(parameters)
		reporter   = gopter.NewFormatedReporter(true, 160, os.Stdout)
		fakeStart  = time.Date(2017, time.February, 13, 15, 30, 10, 0, time.Local)
		rng        = rand.New(rand.NewSource(seed))
	)

	parameters.MinSuccessfulTests = minSuccessfulTests
	parameters.Rng.Seed(seed)

	props.Property(
		"Node can bootstrap all data from filesetfiles, snapshotfiles, and commit log files", prop.ForAll(
			func(input propTestInput) (bool, error) {
				// Test setup
				var (
					// Round to a second to prevent interactions between the RPC client
					// and the node itself when blocksize is not rounded down to a second.
					ns1BlockSize       = input.blockSize.Round(time.Second)
					commitLogBlockSize = 15 * time.Minute
					// Make sure randomly generated data never falls out of retention
					// during the course of a test.
					retentionPeriod = maxBlockSize * 5
					bufferPast      = input.bufferPast
					bufferFuture    = input.bufferFuture
					ns1ROpts        = retention.NewOptions().
							SetRetentionPeriod(retentionPeriod).
							SetBlockSize(ns1BlockSize).
							SetBufferPast(bufferPast).
							SetBufferFuture(bufferFuture)
					nsID      = testNamespaces[0]
					numPoints = input.numPoints
				)

				if bufferPast > ns1BlockSize {
					bufferPast = ns1BlockSize - 1
					ns1ROpts = ns1ROpts.SetBufferPast(bufferPast)
				}
				if bufferFuture > ns1BlockSize {
					bufferFuture = ns1BlockSize - 1
					ns1ROpts = ns1ROpts.SetBufferFuture(bufferFuture)
				}

				if err := ns1ROpts.Validate(); err != nil {
					return false, err
				}

				ns1Opts := namespace.NewOptions().
					SetRetentionOptions(ns1ROpts).
					SetSnapshotEnabled(true)
				ns1, err := namespace.NewMetadata(nsID, ns1Opts)
				if err != nil {
					return false, err
				}
				opts := newTestOptions(t).
					SetCommitLogRetentionPeriod(retentionPeriod).
					SetCommitLogBlockSize(commitLogBlockSize).
					SetNamespaces([]namespace.Metadata{ns1})

				// Test setup
				setup := newTestSetupWithCommitLogAndFilesystemBootstrapper(t, opts)
				defer setup.close()

				log := setup.storageOpts.InstrumentOptions().Logger()
				log.Infof("blockSize: %s\n", ns1ROpts.BlockSize().String())
				log.Infof("bufferPast: %s\n", ns1ROpts.BufferPast().String())
				log.Infof("bufferFuture: %s\n", ns1ROpts.BufferFuture().String())

				setup.setNowFn(fakeStart)

				var (
					ids        = &idGen{longTestID}
					datapoints = generateDatapoints(fakeStart, numPoints, ids)
					// Used to keep track of which datapoints have been written already.
					lastDatapointsIdx = 0
					earliestToCheck   = datapoints[0].time.Truncate(ns1BlockSize)
					latestToCheck     = datapoints[len(datapoints)-1].time.Add(ns1BlockSize)
					timesToRestart    = []time.Time{}
					start             = earliestToCheck
					filePathPrefix    = setup.storageOpts.CommitLogOptions().FilesystemOptions().FilePathPrefix()
				)

				// Generate randomly selected times during which the node will restart
				// and bootstrap before continuing to write data.
				for {
					if start.After(latestToCheck) || start.Equal(latestToCheck) {
						break
					}

					timesToRestart = append(timesToRestart, start)
					start = start.Add(time.Duration(rng.Intn(int(maxBlockSize))))
				}
				timesToRestart = append(timesToRestart, latestToCheck)

				for _, timeToCheck := range timesToRestart {
					startServerWithNewInspection(t, opts, setup)
					ctx := context.NewContext()
					defer ctx.Close()

					log.Infof("writing datapoints")
					var i int
					for i = lastDatapointsIdx; i < len(datapoints); i++ {
						var (
							dp = datapoints[i]
							ts = dp.time
						)
						if !ts.Before(timeToCheck) {
							break
						}

						setup.setNowFn(ts)

						err := setup.db.Write(ctx, nsID, dp.series, ts, dp.value, xtime.Second, nil)
						if err != nil {
							return false, err
						}
					}
					lastDatapointsIdx = i
					log.Infof("wrote datapoints")

					expectedSeriesMap := datapoints[:lastDatapointsIdx].toSeriesMap(ns1BlockSize)
					log.Infof("verifying data in database equals expected data")
					err := verifySeriesMapsReturnError(t, setup, nsID, expectedSeriesMap)
					if err != nil {
						return false, err
					}
					log.Infof("verified data in database equals expected data")
					if input.waitForFlushFiles {
						log.Infof("Waiting for data files to be flushed")
						now := setup.getNowFn()
						latestFlushTime := now.Truncate(ns1BlockSize).Add(-ns1BlockSize)
						expectedFlushedData := datapoints.before(latestFlushTime.Add(-bufferPast)).toSeriesMap(ns1BlockSize)
						err := waitUntilDataFilesFlushed(
							filePathPrefix, setup.shardSet, nsID, expectedFlushedData, 10*time.Second)
						if err != nil {
							return false, err
						}
					}

					if input.waitForSnapshotFiles {
						log.Infof("Waiting for snapshot files to be written")
						now := setup.getNowFn()
						snapshotBlock := now.Add(-bufferPast).Truncate(ns1BlockSize)
						require.NoError(t,
							waitUntilSnapshotFilesFlushed(
								filePathPrefix,
								setup.shardSet,
								nsID,
								[]time.Time{snapshotBlock}, 10*time.Second))
					}

					require.NoError(t, setup.stopServer())
					// Create a new test setup because databases do not have a completely
					// clean shutdown, so they can end up in a bad state where the persist
					// manager is not idle and thus no more flushes can be done, even if
					// there are no other in-progress flushes.
					oldNow := setup.getNowFn()
					setup = newTestSetupWithCommitLogAndFilesystemBootstrapper(
						// FilePathPrefix is randomly generated if not provided, so we need
						// to make sure all our test setups have the same prefix so that
						// they can find each others files.
						t, opts.SetFilePathPrefix(filePathPrefix))
					// Make sure the new setup has the same system time as the previous one.
					setup.setNowFn(oldNow)
				}

				if lastDatapointsIdx != len(datapoints) {
					return false, fmt.Errorf(
						"expected lastDatapointsIdx to be: %d but was: %d", len(datapoints), lastDatapointsIdx)
				}

				return true, nil
			}, genPropTestInputs(fakeStart),
		))

	if !props.Run(reporter) {
		t.Errorf(
			"failed with initial seed: %d and startTime: %d",
			seed, fakeStart.UnixNano())
	}
}

func genPropTestInputs(blockStart time.Time) gopter.Gen {
	return gopter.CombineGens(
		gen.Int64Range(1, int64(maxBlockSize/2)*2),
		gen.Int64Range(1, int64(maxBlockSize/2)*2),
		gen.Int64Range(1, int64(maxBlockSize/2)*2),
		gen.IntRange(0, maxPoints),
		gen.Bool(),
		gen.Bool(),
	).Map(func(val interface{}) propTestInput {
		inputs := val.([]interface{})
		return propTestInput{
			blockSize:            time.Duration(inputs[0].(int64)),
			bufferPast:           time.Duration(inputs[1].(int64)),
			bufferFuture:         time.Duration(inputs[2].(int64)),
			numPoints:            inputs[3].(int),
			waitForFlushFiles:    inputs[4].(bool),
			waitForSnapshotFiles: inputs[5].(bool),
		}
	})
}

type propTestInput struct {
	blockSize            time.Duration
	bufferPast           time.Duration
	bufferFuture         time.Duration
	numPoints            int
	waitForFlushFiles    bool
	waitForSnapshotFiles bool
}

func verifySeriesMapsReturnError(
	t *testing.T,
	ts *testSetup,
	namespace ident.ID,
	seriesMaps map[xtime.UnixNano]generate.SeriesBlock,
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()

	verifySeriesMaps(t, ts, namespace, seriesMaps)
	return nil
}