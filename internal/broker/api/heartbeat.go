/*
Copyright 2026 Politecnico di Torino - NetGroup.

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

package api

import (
	"errors"
	"fmt"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// -----------------------------------------------------------------------------
// 7.3.3 — POST /api/v1/heartbeat   (Consumer Agent → Broker, every 15 s)
// -----------------------------------------------------------------------------

// handleHeartbeat persists the consumer cluster's `liqoClusterId` in the
// in-memory ConsumerRegistry so the Reservation handler can stamp it onto
// new Reservation CRs. Heartbeats also act as the consumer's liveness
// signal, mirroring the role advertisements play for providers.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	requestID := r.Header.Get(HeaderRequestID)
	clusterID := ClusterIDFromContext(r.Context())

	var req HeartbeatRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: err.Error(), RequestID: requestID,
		})
		return
	}
	if err := validateHeartbeatRequest(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: err.Error(), RequestID: requestID,
		})
		return
	}
	if req.ClusterID != clusterID {
		writeError(w, http.StatusForbidden, ErrorResponse{
			Code: ErrCodeForbidden,
			Message: fmt.Sprintf("body.clusterId %q does not match certificate CN %q",
				req.ClusterID, clusterID),
			RequestID: requestID,
		})
		return
	}

	s.consumers.Touch(req.ClusterID, req.LiqoClusterID)

	writeJSON(w, http.StatusOK, HeartbeatResponse{AckAt: metav1.Now()})
}

func validateHeartbeatRequest(req *HeartbeatRequest) error {
	switch {
	case req.ClusterID == "":
		return errors.New("clusterId is required")
	case req.LiqoClusterID == "":
		return errors.New("liqoClusterId is required")
	}
	return nil
}
