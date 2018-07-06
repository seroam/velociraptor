package api

import (
	"errors"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"path"
	"strings"
	api_proto "www.velocidex.com/golang/velociraptor/api/proto"
	config "www.velocidex.com/golang/velociraptor/config"
	"www.velocidex.com/golang/velociraptor/constants"
	crypto_proto "www.velocidex.com/golang/velociraptor/crypto/proto"
	"www.velocidex.com/golang/velociraptor/datastore"
	flows "www.velocidex.com/golang/velociraptor/flows"
	"www.velocidex.com/golang/velociraptor/responder"
	urns "www.velocidex.com/golang/velociraptor/urns"
)

func getFlows(
	config_obj *config.Config,
	client_id string,
	offset uint64, length uint64) (*api_proto.ApiFlowResponse, error) {

	result := &api_proto.ApiFlowResponse{}
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return nil, err
	}

	flow_urns, err := db.ListChildren(
		config_obj, urns.BuildURN(client_id, "flows"),
		offset, length)
	if err != nil {
		return nil, err
	}

	for _, urn := range flow_urns {
		flow_obj, err := flows.GetAFF4FlowObject(config_obj, urn)
		if err != nil {
			return nil, err
		}

		item := &api_proto.ApiFlow{
			Urn:        urn,
			ClientId:   client_id,
			FlowId:     path.Base(urn),
			Name:       flow_obj.RunnerArgs.FlowName,
			RunnerArgs: flow_obj.RunnerArgs,
			Context:    flow_obj.FlowContext,
		}

		result.Items = append(result.Items, item)
	}
	return result, nil
}

func getFlowDetails(
	config_obj *config.Config,
	client_id string, flow_id string) (*api_proto.ApiFlow, error) {

	flow_urn, err := validateFlowId(client_id, flow_id)
	if err != nil {
		return nil, err
	}

	flow_obj, err := flows.GetAFF4FlowObject(config_obj, *flow_urn)
	if err != nil {
		return nil, err
	}

	return &api_proto.ApiFlow{
		Urn:        *flow_urn,
		ClientId:   client_id,
		FlowId:     flow_id,
		Name:       flow_obj.RunnerArgs.FlowName,
		RunnerArgs: flow_obj.RunnerArgs,
		Context:    flow_obj.FlowContext,
	}, nil
}

func getFlowLog(
	config_obj *config.Config,
	client_id string, flow_id string,
	offset uint64, length uint64) (*api_proto.ApiFlowLogDetails, error) {
	flow_urn, err := validateFlowId(client_id, flow_id)
	if err != nil {
		return nil, err
	}

	flow_obj, err := flows.GetAFF4FlowObject(config_obj, *flow_urn)
	if err != nil {
		return nil, err
	}

	result := &api_proto.ApiFlowLogDetails{}
	for idx, item := range flow_obj.FlowContext.Logs {
		if uint64(idx) < offset {
			continue
		}

		if uint64(idx) > offset+length {
			break
		}

		result.Items = append(result.Items, item)
	}

	return result, nil
}

func getFlowRequests(
	config_obj *config.Config,
	client_id string, flow_id string,
	offset uint64, count uint64) (*api_proto.ApiFlowRequestDetails, error) {

	if count == 0 {
		count = 50
	}
	result := &api_proto.ApiFlowRequestDetails{}

	session_id := urns.BuildURN(client_id, "flows", flow_id)
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return nil, err
	}

	requests, err := db.GetClientTasks(config_obj, client_id, true)
	if err != nil {
		return nil, err
	}

	for idx, request := range requests {
		if idx < int(offset) {
			continue
		}

		if idx > int(offset+count) {
			break
		}

		if request.SessionId == session_id {
			args := responder.ExtractGrrMessagePayload(request)
			payload, err := ptypes.MarshalAny(args)
			if err != nil {
				return nil, err
			}
			request.Payload = payload
			request.Args = nil
			request.ArgsRdfName = ""
			result.Items = append(result.Items, request)
		}
	}

	return result, nil
}

func getFlowResults(
	config_obj *config.Config,
	client_id string, flow_id string,
	offset uint64, count uint64) (*api_proto.ApiFlowResultDetails, error) {

	if count == 0 {
		count = 50
	}

	result := &api_proto.ApiFlowResultDetails{}

	urn := urns.BuildURN(client_id, "flows", flow_id, "results")
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return nil, err
	}

	data, err := db.GetSubjectData(config_obj, urn, offset, count)
	if err != nil {
		return nil, err
	}

	for i := offset; i < offset+count; i++ {
		predicate := fmt.Sprintf("%s/%d", constants.FLOW_RESULT, i)
		serialized_message, pres := data[predicate]
		if pres {
			message := &crypto_proto.GrrMessage{}
			err := proto.Unmarshal(serialized_message, message)
			if err != nil {
				return nil, err
			}

			args := responder.ExtractGrrMessagePayload(message)
			payload, err := ptypes.MarshalAny(args)
			if err != nil {
				return nil, err
			}
			message.Payload = payload
			message.Args = nil
			message.ArgsRdfName = ""
			result.Items = append(result.Items, message)
		}
	}

	return result, nil
}

func getFlowDescriptors() (*api_proto.FlowDescriptors, error) {
	result := &api_proto.FlowDescriptors{}
	for _, item := range flows.GetDescriptors() {
		result.Items = append(result.Items, item)
	}

	return result, nil
}

func validateFlowId(client_id string, flow_id string) (*string, error) {
	base_flow := path.Base(flow_id)
	if !strings.HasPrefix(base_flow, constants.FLOW_PREFIX) {
		return nil, errors.New(
			"Flows must start with " + constants.FLOW_PREFIX)
	}

	rebuild_urn := urns.BuildURN(client_id, "flows", base_flow)
	if strings.HasPrefix(flow_id, "aff4:") {
		if flow_id != rebuild_urn {
			return nil, errors.New("Invalid flow id.")
		}
	}

	return &rebuild_urn, nil
}
