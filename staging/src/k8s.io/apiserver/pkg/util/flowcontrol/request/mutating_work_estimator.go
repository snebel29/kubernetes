/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package request

import (
	"math"
	"net/http"
	"time"

	apirequest "k8s.io/apiserver/pkg/endpoints/request"
)

const (
	watchesPerSeat          = 10.0
	eventAdditionalDuration = 5 * time.Millisecond
	// TODO(wojtekt): Remove it once we tune the algorithm to not fail
	// scalability tests.
	enableMutatingWorkEstimator = false
)

func newMutatingWorkEstimator(countFn watchCountGetterFunc) WorkEstimatorFunc {
	return newTestMutatingWorkEstimator(countFn, enableMutatingWorkEstimator)
}

func newTestMutatingWorkEstimator(countFn watchCountGetterFunc, enabled bool) WorkEstimatorFunc {
	estimator := &mutatingWorkEstimator{
		countFn: countFn,
		enabled: enabled,
	}
	return estimator.estimate
}

type mutatingWorkEstimator struct {
	countFn watchCountGetterFunc
	enabled bool
}

func (e *mutatingWorkEstimator) estimate(r *http.Request) WorkEstimate {
	if !e.enabled {
		return WorkEstimate{
			InitialSeats: 1,
		}
	}

	requestInfo, ok := apirequest.RequestInfoFrom(r.Context())
	if !ok {
		// no RequestInfo should never happen, but to be on the safe side
		// let's return a large value.
		return WorkEstimate{
			InitialSeats:      1,
			FinalSeats:        maximumSeats,
			AdditionalLatency: eventAdditionalDuration,
		}
	}
	watchCount := e.countFn(requestInfo)

	// The cost of the request associated with the watchers of that event
	// consists of three parts:
	// - cost of going through the event change logic
	// - cost of serialization of the event
	// - cost of processing an event object for each watcher (e.g. filtering,
	//     sending data over network)
	// We're starting simple to get some operational experience with it and
	// we will work on tuning the algorithm later. Given that the actual work
	// associated with processing watch events is happening in multiple
	// goroutines (proportional to the number of watchers) that are all
	// resumed at once, as a starting point we assume that each such goroutine
	// is taking 1/Nth of a seat for M milliseconds.
	// We allow the accounting of that work in P&F to be reshaped into another
	// rectangle of equal area for practical reasons.
	var finalSeats uint
	var additionalLatency time.Duration

	// TODO: Make this unconditional after we tune the algorithm better.
	//   Technically, there is an overhead connected to processing an event after
	//   the request finishes even if there is a small number of watches.
	//   However, until we tune the estimation we want to stay on the safe side
	//   an avoid introducing additional latency for almost every single request.
	if watchCount >= watchesPerSeat {
		// TODO: As described in the KEP, we should take into account that not all
		//   events are equal and try to estimate the cost of a single event based on
		//   some historical data about size of events.
		finalSeats = uint(math.Ceil(float64(watchCount) / watchesPerSeat))
		finalWork := SeatsTimesDuration(float64(finalSeats), eventAdditionalDuration)

		// While processing individual events is highly parallel,
		// the design/implementation of P&F has a couple limitations that
		// make using this assumption in the P&F implementation very
		// inefficient because:
		// - we reserve max(initialSeats, finalSeats) for time of executing
		//   both phases of the request
		// - even more importantly, when a given `wide` request is the one to
		//   be dispatched, we are not dispatching any other request until
		//   we accumulate enough seats to dispatch the nominated one, even
		//   if currently unoccupied seats would allow for dispatching some
		//   other requests in the meantime
		// As a consequence of these, the wider the request, the more capacity
		// will effectively be blocked and unused during dispatching and
		// executing this request.
		//
		// To mitigate the impact of it, we're capping the maximum number of
		// seats that can be assigned to a given request. Thanks to it:
		// 1) we reduce the amount of seat-seconds that are "wasted" during
		//    dispatching and executing initial phase of the request
		// 2) we are not changing the finalWork estimate - just potentially
		//    reshaping it to be narrower and longer. As long as the maximum
		//    seats setting will prevent dispatching too many requests at once
		//    to prevent overloading kube-apiserver (and/or etcd or the VM or
		//    a physical machine it is running on), we believe the relaxed
		//    version should be good enough to achieve the P&F goals.
		//
		// TODO: Confirm that the current cap of maximumSeats allow us to
		//   achieve the above.
		if finalSeats > maximumSeats {
			finalSeats = maximumSeats
		}
		additionalLatency = finalWork.DurationPerSeat(float64(finalSeats))
	}

	return WorkEstimate{
		InitialSeats:      1,
		FinalSeats:        finalSeats,
		AdditionalLatency: additionalLatency,
	}
}
