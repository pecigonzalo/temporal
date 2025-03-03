// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
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

package hybrid_logical_clock

import (
	clockpb "go.temporal.io/server/api/clock/v1"
	commonclock "go.temporal.io/server/common/clock"
)

type Clock = clockpb.HybridLogicalClock

// Next generates the next clock timestamp given the current clock.
// HybridLogicalClock requires the previous clock to ensure that time doesn't move backwards and the next clock is
// monotonically increasing.
func Next(clock Clock, source commonclock.TimeSource) Clock {
	wallclock := source.Now().UnixMilli()
	// Ensure time does not move backwards
	if wallclock < clock.GetWallClock() {
		wallclock = clock.GetWallClock()
	}
	// Ensure timestamp is monotonically increasing
	if wallclock == clock.GetWallClock() {
		clock.Version = clock.GetVersion() + 1
	} else {
		clock.Version = 0
		clock.WallClock = wallclock
	}

	return Clock{WallClock: wallclock, Version: clock.Version, ClusterId: clock.ClusterId}
}

// Zero generates a zeroed logical clock for the cluster ID.
func Zero(clusterID int64) Clock {
	return Clock{WallClock: 0, Version: 0, ClusterId: clusterID}
}

func sign[T int64 | int32](x T) int {
	if x > 0 {
		return 1
	}
	if x < 0 {
		return -1
	}
	return 0
}

// Compare 2 clocks, returns 0 if a == b, -1 if a > b, 1 if a < b
func Compare(a Clock, b Clock) int {
	if a.WallClock == b.WallClock {
		if a.Version == b.Version {
			return sign(b.ClusterId - a.ClusterId)
		}
		return sign(b.Version - a.Version)
	}
	return sign(b.WallClock - a.WallClock)
}

// Greater returns true if a is greater than b
func Greater(a Clock, b Clock) bool {
	return Compare(b, a) > 0
}

// Greater returns true if a is greater than b
func Less(a Clock, b Clock) bool {
	return Compare(a, b) > 0
}

// Max returns the maximum of two clocks
func Max(a Clock, b Clock) Clock {
	if Compare(a, b) > 0 {
		return b
	}
	return a
}

// Equal returns whether two clocks are equal
func Equal(a Clock, b Clock) bool {
	return Compare(a, b) == 0
}
