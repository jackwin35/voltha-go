/*
 * Copyright 2018-present Open Networking Foundation

 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at

 * http://www.apache.org/licenses/LICENSE-2.0

 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package device

import (
	"context"
	"encoding/hex"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/opencord/voltha-go/db/model"
	"github.com/opencord/voltha-go/rw_core/core/device/flow"
	"github.com/opencord/voltha-go/rw_core/core/device/group"
	"github.com/opencord/voltha-go/rw_core/core/device/logical_port"
	"github.com/opencord/voltha-go/rw_core/core/device/meter"
	fd "github.com/opencord/voltha-go/rw_core/flowdecomposition"
	"github.com/opencord/voltha-go/rw_core/route"
	coreutils "github.com/opencord/voltha-go/rw_core/utils"
	fu "github.com/opencord/voltha-lib-go/v3/pkg/flows"
	"github.com/opencord/voltha-lib-go/v3/pkg/log"
	ic "github.com/opencord/voltha-protos/v3/go/inter_container"
	ofp "github.com/opencord/voltha-protos/v3/go/openflow_13"
	"github.com/opencord/voltha-protos/v3/go/voltha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LogicalAgent represent attributes of logical device agent
type LogicalAgent struct {
	logicalDeviceID string
	serialNumber    string
	rootDeviceID    string
	deviceMgr       *Manager
	ldeviceMgr      *LogicalManager
	ldProxy         *model.Proxy
	stopped         bool
	deviceRoutes    *route.DeviceRoutes
	flowDecomposer  *fd.FlowDecomposer
	defaultTimeout  time.Duration
	logicalDevice   *voltha.LogicalDevice
	requestQueue    *coreutils.RequestQueue
	orderedEvents   orderedEvents
	startOnce       sync.Once
	stopOnce        sync.Once

	flowLoader  *flow.Loader
	meterLoader *meter.Loader
	groupLoader *group.Loader
	portLoader  *port.Loader
}

func newLogicalAgent(id string, sn string, deviceID string, ldeviceMgr *LogicalManager,
	deviceMgr *Manager, dbProxy *model.Path, ldProxy *model.Proxy, defaultTimeout time.Duration) *LogicalAgent {
	agent := &LogicalAgent{
		logicalDeviceID: id,
		serialNumber:    sn,
		rootDeviceID:    deviceID,
		deviceMgr:       deviceMgr,
		ldProxy:         ldProxy,
		ldeviceMgr:      ldeviceMgr,
		flowDecomposer:  fd.NewFlowDecomposer(deviceMgr),
		defaultTimeout:  defaultTimeout,
		requestQueue:    coreutils.NewRequestQueue(),

		flowLoader:  flow.NewLoader(dbProxy.SubPath("logical_flows").Proxy(id)),
		groupLoader: group.NewLoader(dbProxy.SubPath("logical_groups").Proxy(id)),
		meterLoader: meter.NewLoader(dbProxy.SubPath("logical_meters").Proxy(id)),
		portLoader:  port.NewLoader(dbProxy.SubPath("logical_ports").Proxy(id)),
	}
	agent.deviceRoutes = route.NewDeviceRoutes(agent.logicalDeviceID, agent.deviceMgr.getDevice)
	return agent
}

// start creates the logical device and add it to the data model
func (agent *LogicalAgent) start(ctx context.Context, loadFromDB bool) error {
	needToStart := false
	if agent.startOnce.Do(func() { needToStart = true }); !needToStart {
		return nil
	}

	logger.Infow("starting-logical_device-agent", log.Fields{"logical-device-id": agent.logicalDeviceID, "load-from-db": loadFromDB})

	var startSucceeded bool
	defer func() {
		if !startSucceeded {
			if err := agent.stop(ctx); err != nil {
				logger.Errorw("failed-to-cleanup-after-unsuccessful-start", log.Fields{"logical-device-id": agent.logicalDeviceID, "error": err})
			}
		}
	}()

	var ld *voltha.LogicalDevice
	if !loadFromDB {
		//Build the logical device based on information retrieved from the device adapter
		var switchCap *ic.SwitchCapability
		var err error
		if switchCap, err = agent.deviceMgr.getSwitchCapability(ctx, agent.rootDeviceID); err != nil {
			return err
		}
		ld = &voltha.LogicalDevice{Id: agent.logicalDeviceID, RootDeviceId: agent.rootDeviceID}

		// Create the datapath ID (uint64) using the logical device ID (based on the MAC Address)
		var datapathID uint64
		if datapathID, err = coreutils.CreateDataPathID(agent.serialNumber); err != nil {
			return err
		}
		ld.DatapathId = datapathID
		ld.Desc = (proto.Clone(switchCap.Desc)).(*ofp.OfpDesc)
		logger.Debugw("Switch-capability", log.Fields{"Desc": ld.Desc, "fromAd": switchCap.Desc})
		ld.SwitchFeatures = (proto.Clone(switchCap.SwitchFeatures)).(*ofp.OfpSwitchFeatures)
		ld.Flows = &ofp.Flows{Items: nil}
		ld.FlowGroups = &ofp.FlowGroups{Items: nil}
		ld.Ports = []*voltha.LogicalPort{}

		// Save the logical device
		if err := agent.ldProxy.Set(ctx, ld.Id, ld); err != nil {
			logger.Errorw("failed-to-add-logical-device", log.Fields{"logical-device-id": agent.logicalDeviceID})
			return err
		}
		logger.Debugw("logicaldevice-created", log.Fields{"logical-device-id": agent.logicalDeviceID, "root-id": ld.RootDeviceId})

		agent.logicalDevice = ld

		// Setup the logicalports - internal processing, no need to propagate the client context
		go func() {
			err := agent.setupLogicalPorts(context.Background())
			if err != nil {
				logger.Errorw("unable-to-setup-logical-ports", log.Fields{"error": err})
			}
		}()
	} else {
		//	load from dB - the logical may not exist at this time.  On error, just return and the calling function
		// will destroy this agent.
		ld := &voltha.LogicalDevice{}
		have, err := agent.ldProxy.Get(ctx, agent.logicalDeviceID, ld)
		if err != nil {
			return err
		} else if !have {
			return status.Errorf(codes.NotFound, "logical_device-%s", agent.logicalDeviceID)
		}

		// Update the root device Id
		agent.rootDeviceID = ld.RootDeviceId

		// Update the last data
		agent.logicalDevice = ld

		// load the flows, meters and groups from KV to cache
		agent.flowLoader.Load(ctx)
		agent.meterLoader.Load(ctx)
		agent.groupLoader.Load(ctx)
		agent.portLoader.Load(ctx)
	}

	// Setup the device routes. Building routes may fail if the pre-conditions are not satisfied (e.g. no PON ports present)
	if loadFromDB {
		go func() {
			if err := agent.buildRoutes(context.Background()); err != nil {
				logger.Warn("routes-not-ready", log.Fields{"logical-device-id": agent.logicalDeviceID, "error": err})
			}
		}()
	}
	startSucceeded = true

	return nil
}

// stop stops the logical device agent.  This removes the logical device from the data model.
func (agent *LogicalAgent) stop(ctx context.Context) error {
	var returnErr error
	agent.stopOnce.Do(func() {
		logger.Info("stopping-logical_device-agent")

		if err := agent.requestQueue.WaitForGreenLight(ctx); err != nil {
			// This should never happen - an error is returned only if the agent is stopped and an agent is only stopped once.
			returnErr = err
			return
		}
		defer agent.requestQueue.RequestComplete()

		//Remove the logical device from the model
		if err := agent.ldProxy.Remove(ctx, agent.logicalDeviceID); err != nil {
			returnErr = err
		} else {
			logger.Debugw("logicaldevice-removed", log.Fields{"logicaldeviceId": agent.logicalDeviceID})
		}
		// TODO: remove all entries from all loaders
		// TODO: don't allow any more modifications to flows/groups/meters/ports or to any logical device field

		agent.stopped = true

		logger.Info("logical_device-agent-stopped")
	})
	return returnErr
}

// GetLogicalDevice returns the latest logical device data
func (agent *LogicalAgent) GetLogicalDevice(ctx context.Context) (*voltha.LogicalDevice, error) {
	if err := agent.requestQueue.WaitForGreenLight(ctx); err != nil {
		return nil, err
	}
	defer agent.requestQueue.RequestComplete()
	return proto.Clone(agent.logicalDevice).(*voltha.LogicalDevice), nil
}

func (agent *LogicalAgent) addFlowsAndGroupsToDevices(deviceRules *fu.DeviceRules, flowMetadata *voltha.FlowMetadata) []coreutils.Response {
	logger.Debugw("send-add-flows-to-device-manager", log.Fields{"logicalDeviceID": agent.logicalDeviceID, "deviceRules": deviceRules, "flowMetadata": flowMetadata})

	responses := make([]coreutils.Response, 0)
	for deviceID, value := range deviceRules.GetRules() {
		response := coreutils.NewResponse()
		responses = append(responses, response)
		go func(deviceId string, value *fu.FlowsAndGroups) {
			subCtx, cancel := context.WithTimeout(context.Background(), agent.defaultTimeout)
			defer cancel()
			start := time.Now()
			if err := agent.deviceMgr.addFlowsAndGroups(subCtx, deviceId, value.ListFlows(), value.ListGroups(), flowMetadata); err != nil {
				logger.Errorw("flow-add-failed", log.Fields{"deviceID": deviceId, "error": err, "wait-time": time.Since(start)})
				response.Error(status.Errorf(codes.Internal, "flow-add-failed: %s", deviceId))
			}
			response.Done()
		}(deviceID, value)
	}
	// Return responses (an array of channels) for the caller to wait for a response from the far end.
	return responses
}

func (agent *LogicalAgent) deleteFlowsAndGroupsFromDevices(deviceRules *fu.DeviceRules, flowMetadata *voltha.FlowMetadata) []coreutils.Response {
	logger.Debugw("send-delete-flows-to-device-manager", log.Fields{"logicalDeviceID": agent.logicalDeviceID})

	responses := make([]coreutils.Response, 0)
	for deviceID, value := range deviceRules.GetRules() {
		response := coreutils.NewResponse()
		responses = append(responses, response)
		go func(deviceId string, value *fu.FlowsAndGroups) {
			subCtx, cancel := context.WithTimeout(context.Background(), agent.defaultTimeout)
			defer cancel()
			start := time.Now()
			if err := agent.deviceMgr.deleteFlowsAndGroups(subCtx, deviceId, value.ListFlows(), value.ListGroups(), flowMetadata); err != nil {
				logger.Errorw("flow-delete-failed", log.Fields{"deviceID": deviceId, "error": err, "wait-time": time.Since(start)})
				response.Error(status.Errorf(codes.Internal, "flow-delete-failed: %s", deviceId))
			}
			response.Done()
		}(deviceID, value)
	}
	return responses
}

func (agent *LogicalAgent) updateFlowsAndGroupsOfDevice(deviceRules *fu.DeviceRules, flowMetadata *voltha.FlowMetadata) []coreutils.Response {
	logger.Debugw("send-update-flows-to-device-manager", log.Fields{"logicalDeviceID": agent.logicalDeviceID})

	responses := make([]coreutils.Response, 0)
	for deviceID, value := range deviceRules.GetRules() {
		response := coreutils.NewResponse()
		responses = append(responses, response)
		go func(deviceId string, value *fu.FlowsAndGroups) {
			subCtx, cancel := context.WithTimeout(context.Background(), agent.defaultTimeout)
			defer cancel()
			if err := agent.deviceMgr.updateFlowsAndGroups(subCtx, deviceId, value.ListFlows(), value.ListGroups(), flowMetadata); err != nil {
				logger.Errorw("flow-update-failed", log.Fields{"deviceID": deviceId, "error": err})
				response.Error(status.Errorf(codes.Internal, "flow-update-failed: %s", deviceId))
			}
			response.Done()
		}(deviceID, value)
	}
	return responses
}

func (agent *LogicalAgent) deleteFlowsFromParentDevice(flows map[uint64]*ofp.OfpFlowStats, metadata *voltha.FlowMetadata) []coreutils.Response {
	logger.Debugw("deleting-flows-from-parent-device", log.Fields{"logical-device-id": agent.logicalDeviceID, "flows": flows})
	responses := make([]coreutils.Response, 0)
	for _, flow := range flows {
		response := coreutils.NewResponse()
		responses = append(responses, response)
		uniPort, err := agent.getUNILogicalPortNo(flow)
		if err != nil {
			logger.Error("no-uni-port-in-flow", log.Fields{"deviceID": agent.rootDeviceID, "flow": flow, "error": err})
			response.Error(err)
			response.Done()
			continue
		}
		logger.Debugw("uni-port", log.Fields{"flows": flows, "uni-port": uniPort})
		go func(uniPort uint32, metadata *voltha.FlowMetadata) {
			subCtx, cancel := context.WithTimeout(context.Background(), agent.defaultTimeout)
			defer cancel()
			if err := agent.deviceMgr.deleteParentFlows(subCtx, agent.rootDeviceID, uniPort, metadata); err != nil {
				logger.Error("flow-delete-failed", log.Fields{"device-id": agent.rootDeviceID, "error": err})
				response.Error(status.Errorf(codes.Internal, "flow-delete-failed: %s %v", agent.rootDeviceID, err))
			}
			response.Done()
		}(uniPort, metadata)
	}
	return responses
}

func (agent *LogicalAgent) packetOut(ctx context.Context, packet *ofp.OfpPacketOut) {
	logger.Debugw("packet-out", log.Fields{
		"packet": hex.EncodeToString(packet.Data),
		"inPort": packet.GetInPort(),
	})
	outPort := fu.GetPacketOutPort(packet)
	//frame := packet.GetData()
	//TODO: Use a channel between the logical agent and the device agent
	if err := agent.deviceMgr.packetOut(ctx, agent.rootDeviceID, outPort, packet); err != nil {
		logger.Error("packetout-failed", log.Fields{"logicalDeviceID": agent.rootDeviceID})
	}
}

func (agent *LogicalAgent) packetIn(port uint32, transactionID string, packet []byte) {
	logger.Debugw("packet-in", log.Fields{
		"port":          port,
		"packet":        hex.EncodeToString(packet),
		"transactionId": transactionID,
	})
	packetIn := fu.MkPacketIn(port, packet)
	agent.ldeviceMgr.SendPacketIn(agent.logicalDeviceID, transactionID, packetIn)
	logger.Debugw("sending-packet-in", log.Fields{"packet": hex.EncodeToString(packetIn.Data)})
}
