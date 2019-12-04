// Copyright 2018 Bull S.A.S. Atos Technologies - Bull, Rue Jean Jaures, B.P.68, 78340, Les Clayes-sous-Bois, France.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deployments

import (
	"context"
	"fmt"
	"github.com/ystia/yorc/v4/log"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"

	"github.com/ystia/yorc/v4/deployments/store"
	"github.com/ystia/yorc/v4/events"
	"github.com/ystia/yorc/v4/helper/consulutil"
	"github.com/ystia/yorc/v4/storage"
	storageTypes "github.com/ystia/yorc/v4/storage/types"
	"github.com/ystia/yorc/v4/tosca"
)

// StoreDeploymentDefinition takes a defPath and parse it as a tosca.Topology then it store it in consul under
// consulutil.DeploymentKVPrefix/deploymentID
func StoreDeploymentDefinition(ctx context.Context, deploymentID string, defPath string) error {
	if err := SetDeploymentStatus(ctx, deploymentID, INITIAL); err != nil {
		return handleDeploymentStatus(ctx, deploymentID, err)
	}

	topology := tosca.Topology{}
	definition, err := os.Open(defPath)
	if err != nil {
		return handleDeploymentStatus(ctx, deploymentID, errors.Wrapf(err, "Failed to open definition file %q", defPath))
	}
	defBytes, err := ioutil.ReadAll(definition)
	if err != nil {
		return handleDeploymentStatus(ctx, deploymentID, errors.Wrapf(err, "Failed to open definition file %q", defPath))
	}

	err = yaml.Unmarshal(defBytes, &topology)
	if err != nil {
		return handleDeploymentStatus(ctx, deploymentID, errors.Wrapf(err, "Failed to unmarshal yaml definition for file %q", defPath))
	}

	consulutil.StoreConsulKeyAsString(path.Join(consulutil.DeploymentKVPrefix, deploymentID, "status"), fmt.Sprint(INITIAL))

	err = store.Deployment(ctx, topology, deploymentID, filepath.Dir(defPath))
	if err != nil {
		return handleDeploymentStatus(ctx, deploymentID, errors.Wrapf(err, "Failed to store TOSCA Definition for deployment with id %q, (file path %q)", deploymentID, defPath))
	}
	err = registerImplementationTypes(ctx, deploymentID)
	if err != nil {
		return handleDeploymentStatus(ctx, deploymentID, err)
	}

	// Enhance nodes
	nodes, err := GetNodes(ctx, deploymentID)
	if err != nil {
		return err
	}
	return handleDeploymentStatus(ctx, deploymentID, enhanceNodes(ctx, deploymentID, nodes))
}

func handleDeploymentStatus(ctx context.Context, deploymentID string, err error) error {
	if err != nil {
		SetDeploymentStatus(ctx, deploymentID, DEPLOYMENT_FAILED)
	}
	return err
}

// createInstancesForNode checks if the given node is hosted on a Scalable node, stores the number of required instances and sets the instance's status to INITIAL
func createInstancesForNode(ctx context.Context, consulStore consulutil.ConsulStore, deploymentID, nodeName string) error {
	nbInstances, err := GetDefaultNbInstancesForNode(ctx, deploymentID, nodeName)
	if err != nil {
		return err
	}
	createNodeInstances(consulStore, nbInstances, deploymentID, nodeName)

	// Check for FIPConnectivity capabilities
	is, capabilityNodeName, err := HasAnyRequirementCapability(ctx, deploymentID, nodeName, "network", "yorc.capabilities.openstack.FIPConnectivity")
	if err != nil {
		return err
	}
	if is {
		createNodeInstances(consulStore, nbInstances, deploymentID, capabilityNodeName)
	}

	// Check for Assignable capabilities
	is, capabilityNodeName, err = HasAnyRequirementCapability(ctx, deploymentID, nodeName, "assignment", "yorc.capabilities.Assignable")
	if err != nil {
		return err
	}
	if is {
		createNodeInstances(consulStore, nbInstances, deploymentID, capabilityNodeName)
	}

	bs, bsNames, err := checkBlockStorage(ctx, deploymentID, nodeName)
	if err != nil {
		return err
	}

	if bs {
		for _, name := range bsNames {
			createNodeInstances(consulStore, nbInstances, deploymentID, name)
		}

	}
	return nil
}

func registerImplementationTypes(ctx context.Context, deploymentID string) error {
	// We use synchronous communication with consul here to allow to check for duplicates
	types, err := GetTypes(ctx, deploymentID)
	if err != nil {
		return err
	}
	extensionsMap := make(map[string]string)
	for _, t := range types {
		isImpl, err := IsTypeDerivedFrom(ctx, deploymentID, t, "tosca.artifacts.Implementation")
		if err != nil {
			if IsTypeMissingError(err) {
				// Bypassing this error it may happen in case of an used type let's trust Alien
				events.SimpleLogEntry(events.LogLevelWARN, deploymentID).RegisterAsString(fmt.Sprintf("[WARNING] %s", err))
				continue
			}
			return err
		}
		if isImpl {
			extensions, err := GetArtifactTypeExtensions(ctx, deploymentID, t)
			if err != nil {
				return err
			}

			for _, ext := range extensions {
				ext = strings.ToLower(ext)
				extensionsMap[ext] = t
			}
		}
	}

	if len(extensionsMap) > 0 {
		return storage.GetStore(storageTypes.StoreTypeDeployment).Set(path.Join(consulutil.DeploymentKVPrefix, deploymentID, "topology", implementationArtifactsExtensionsPath), extensionsMap)
	}

	return nil
}

// EnhanceNodes walk through the provided nodes an for each of them if needed it creates the instances and fix alien BlockStorage declaration
func enhanceNodes(ctx context.Context, deploymentID string, nodes []string) error {
	ctxStore, errGroup, consulStore := consulutil.WithContext(ctx)
	computes := make([]string, 0)
	for _, nodeName := range nodes {
		isCompute, err := createInstanceAndFixModel(ctxStore, consulStore, deploymentID, nodeName)
		if err != nil {
			return err
		}
		if isCompute {
			computes = append(computes, nodeName)
		}
	}
	err := createMissingBlockStorageForNodes(ctx, consulStore, deploymentID, computes)
	if err != nil {
		return err
	}
	err = errGroup.Wait()
	if err != nil {
		return err
	}

	_, errGroup, consulStore = consulutil.WithContext(ctx)
	for _, nodeName := range nodes {
		err = createRelationshipInstances(ctx, consulStore, deploymentID, nodeName)
		if err != nil {
			return err
		}
	}

	err = enhanceWorkflows(ctx, consulStore, deploymentID)
	if err != nil {
		return err
	}

	err = enhanceAttributes(ctx, deploymentID, nodes)
	if err != nil {
		return err
	}
	return errGroup.Wait()
}

func createInstanceAndFixModel(ctx context.Context, consulStore consulutil.ConsulStore, deploymentID string, nodeName string) (bool, error) {

	var isCompute bool
	err := fixGetOperationOutputs(ctx, deploymentID, nodeName)
	if err != nil {
		return isCompute, err
	}

	substitutable, err := isSubstitutableNode(ctx, deploymentID, nodeName)
	if err != nil {
		return isCompute, err
	}
	if !substitutable {
		err = createInstancesForNode(ctx, consulStore, deploymentID, nodeName)
		if err != nil {
			return isCompute, err
		}
		err = fixAlienBlockStorages(ctx, deploymentID, nodeName)
		if err != nil {
			return isCompute, err
		}
		isCompute, err = IsNodeDerivedFrom(ctx, deploymentID, nodeName, "tosca.nodes.Compute")
	}

	return isCompute, err
}

func fixGetOperationOutputs(ctx context.Context, deploymentID, nodeName string) error {
	nodeTypeName, err := GetNodeType(ctx, deploymentID, nodeName)
	if err != nil {
		return err
	}
	nodeType := new(tosca.NodeType)
	err = getTypeStruct(deploymentID, nodeTypeName, nodeType)
	if err != nil {
		return err
	}

	// Check attributes definitions
	for _, attributeDef := range nodeType.Attributes {
		err := addOperationOutputOnNodeType(ctx, deploymentID, nodeName, nodeTypeName, attributeDef.Default)
		if err != nil {
			return err
		}
	}

	// Check input value assignments
	for _, interfaceDef := range nodeType.Interfaces {
		for _, operationDef := range interfaceDef.Operations {
			for _, inputDef := range operationDef.Inputs {
				err := addOperationOutputOnNodeType(ctx, deploymentID, nodeName, nodeTypeName, inputDef.ValueAssign)
				if err != nil {
					return err
				}
			}
		}
	}

	// Check requirements
	for reqIndex := range nodeType.Requirements {
		ind := strconv.Itoa(reqIndex)
		relationshipTypeName, err := GetRelationshipForRequirement(ctx, deploymentID, nodeName, ind)
		if err != nil {
			return err
		}
		if relationshipTypeName == "" {
			continue
		}
		rType := new(tosca.RelationshipType)
		err = getTypeStruct(deploymentID, relationshipTypeName, rType)
		if err != nil {
			return err
		}

		// Check attributes definitions
		for _, attributeDef := range rType.Attributes {
			err := addOperationOutputOnRelationshipType(ctx, deploymentID, nodeName, nodeTypeName, ind, attributeDef.Default)
			if err != nil {
				return err
			}
		}

		// Check input value assignments
		for _, interfaceDef := range rType.Interfaces {
			for _, operationDef := range interfaceDef.Operations {
				for _, inputDef := range operationDef.Inputs {
					err := addOperationOutputOnRelationshipType(ctx, deploymentID, nodeName, nodeTypeName, ind, inputDef.ValueAssign)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

func addOperationOutputOnNodeType(ctx context.Context, deploymentID, nodeName, nodeTypeName string, va *tosca.ValueAssignment) error {
	if va == nil || va.Type != tosca.ValueAssignmentFunction {
		return nil
	}
	f := va.GetFunction()
	if f != nil {
		opOutputFuncs := f.GetFunctionsByOperator(tosca.GetOperationOutputOperator)
		for _, oof := range opOutputFuncs {
			if len(oof.Operands) != 4 {
				return errors.Errorf("Invalid %q TOSCA function: %v", tosca.GetOperationOutputOperator, oof)
			}
			entityName := url.QueryEscape(oof.Operands[0].String())
			interfaceName := oof.Operands[1].String()
			operationName := oof.Operands[2].String()
			outputName := oof.Operands[3].String()

			output := tosca.Output{ValueAssign: &tosca.ValueAssignment{
				Type:  tosca.ValueAssignmentFunction,
				Value: oof.String(),
			}}
			switch entityName {
			case "SELF":
				return storeOperationOutputOnNodeType(ctx, deploymentID, nodeTypeName, interfaceName, operationName, outputName, &output)
			case "HOST":
				hostedOn, err := GetHostedOnNode(ctx, deploymentID, nodeName)
				if err != nil {
					return nil
				} else if hostedOn == "" {
					return errors.New("Fail to get the hostedOn to fix the output")
				}

				hostedNodeType, err := GetNodeType(ctx, deploymentID, hostedOn)
				if err != nil {
					return err
				}
				return storeOperationOutputOnNodeType(ctx, deploymentID, hostedNodeType, interfaceName, operationName, outputName, &output)
			default:
				log.Printf("[WARNING] The entity name:%q for operation output on node is not handled", entityName)
			}
		}
	}
	return nil
}

func addOperationOutputOnRelationshipType(ctx context.Context, deploymentID, nodeName, nodeTypeName, reqIndex string, va *tosca.ValueAssignment) error {
	if va == nil || va.Type != tosca.ValueAssignmentFunction {
		return nil
	}
	f := va.GetFunction()
	if f != nil {
		opOutputFuncs := f.GetFunctionsByOperator(tosca.GetOperationOutputOperator)
		for _, oof := range opOutputFuncs {
			if len(oof.Operands) != 4 {
				return errors.Errorf("Invalid %q TOSCA function: %v", tosca.GetOperationOutputOperator, oof)
			}
			entityName := url.QueryEscape(oof.Operands[0].String())
			interfaceName := oof.Operands[1].String()
			operationName := oof.Operands[2].String()
			outputName := oof.Operands[3].String()

			output := tosca.Output{ValueAssign: &tosca.ValueAssignment{
				Type:  tosca.ValueAssignmentFunction,
				Value: oof.String(),
			}}
			switch entityName {
			case "SOURCE":
				return storeOperationOutputOnRelationshipType(ctx, deploymentID, nodeTypeName, interfaceName, operationName, outputName, &output)
			case "TARGET":
				targetNodeName, err := GetTargetNodeForRequirement(ctx, deploymentID, nodeName, reqIndex)
				if err != nil {
					return err
				}
				targetNodeTypeName, err := GetNodeType(ctx, deploymentID, targetNodeName)
				if err != nil {
					return err
				}
				return storeOperationOutputOnRelationshipType(ctx, deploymentID, targetNodeTypeName, interfaceName, operationName, outputName, &output)
			default:
				log.Printf("[WARNING] The entity name:%q for operation output on relationship is not handled", entityName)
			}
		}
	}
	return nil
}

func storeOperationOutputOnNodeType(ctx context.Context, deploymentID, typeName, interfaceName, operationName, outputName string, output *tosca.Output) error {
	// Retrieve the node type in hierarchy which implements the operation
	typeNameImpl, err := GetTypeImplementingAnOperation(ctx, deploymentID, typeName, fmt.Sprintf("%s.%s", interfaceName, operationName))
	if err != nil {
		return nil
	}

	nodeType := new(tosca.NodeType)
	err = getTypeStruct(deploymentID, typeNameImpl, nodeType)
	if err != nil {
		return err
	}

	if !operationExists(nodeType.Interfaces, interfaceName, operationName) {
		log.Printf("{WARNING] interface (%s) - operation (%s) not found for type:%+v", interfaceName, operationName, nodeType)
		return nil
	}

	op := nodeType.Interfaces[interfaceName].Operations[operationName]
	if op.Outputs == nil {
		op.Outputs = make(map[string]tosca.Output)
	}

	op.Outputs[outputName] = *output
	nodeType.Interfaces[interfaceName].Operations[operationName] = op

	typePath, err := locateTypePath(deploymentID, typeNameImpl)
	if err != nil {
		return err
	}
	return storage.GetStore(storageTypes.StoreTypeDeployment).Set(typePath, nodeType)
}

func operationExists(interfaces map[string]tosca.InterfaceDefinition, interfaceName, operationName string) bool {
	if interfaces == nil {
		return false
	}

	i, exist := interfaces[interfaceName]
	if !exist {
		return false
	}

	if i.Operations == nil {
		return false
	}

	_, exist = i.Operations[operationName]
	if !exist {
		return false
	}
	return true
}

func storeOperationOutputOnRelationshipType(ctx context.Context, deploymentID, typeName, interfaceName, operationName, outputName string, output *tosca.Output) error {
	// Retrieve the node type in hierarchy which implements the operation
	typeNameImpl, err := GetTypeImplementingAnOperation(ctx, deploymentID, typeName, fmt.Sprintf("%s.%s", interfaceName, operationName))
	if err != nil {
		return nil
	}

	relationshipType := new(tosca.RelationshipType)
	err = getTypeStruct(deploymentID, typeNameImpl, relationshipType)
	if err != nil {
		return err
	}

	if !operationExists(relationshipType.Interfaces, interfaceName, operationName) {
		log.Printf("{WARNING] interface (%s) - operation (%s) not found for type:%+v", interfaceName, operationName, relationshipType)
		return nil
	}

	op := relationshipType.Interfaces[interfaceName].Operations[operationName]
	if op.Outputs == nil {
		op.Outputs = make(map[string]tosca.Output)
	}

	op.Outputs[outputName] = *output
	relationshipType.Interfaces[interfaceName].Operations[operationName] = op

	typePath, err := locateTypePath(deploymentID, typeNameImpl)
	if err != nil {
		return err
	}
	return storage.GetStore(storageTypes.StoreTypeDeployment).Set(typePath, relationshipType)
}

// fixAlienBlockStorages rewrites the relationship between a BlockStorage and a Compute to match the TOSCA specification
func fixAlienBlockStorages(ctx context.Context, deploymentID, nodeName string) error {
	isBS, err := IsNodeDerivedFrom(ctx, deploymentID, nodeName, "tosca.nodes.BlockStorage")
	if err != nil {
		return err
	}
	if isBS {
		attachReqs, err := GetRequirementsByTypeForNode(ctx, deploymentID, nodeName, "attachment")
		if err != nil {
			return err
		}
		for _, attachReq := range attachReqs {
			req := attachReq.RequirementAssignment
			// Reverse the target node
			computeNodeName := req.Node
			req.Node = nodeName
			device, err := GetNodePropertyValue(ctx, deploymentID, nodeName, "device")
			if err != nil {
				return errors.Wrapf(err, "Failed to fix Alien-specific BlockStorage %q", nodeName)
			}
			if device != nil {
				va := &tosca.ValueAssignment{}
				if device.RawString() != "" {
					err = yaml.Unmarshal([]byte(device.RawString()), &va)
					if err != nil {
						return errors.Wrapf(err, "Failed to fix Alien-specific BlockStorage %q, failed to parse device property", nodeName)
					}
				}
				// Add device requirement property
				if req.RelationshipProps == nil {
					req.RelationshipProps = make(map[string]*tosca.ValueAssignment)
				}
				req.RelationshipProps["device"] = va
			}

			// Update the compute node with new requirement
			node, err := getNodeTemplateStruct(ctx, deploymentID, computeNodeName)
			if err != nil {
				return err
			}

			reqMap := make(map[string]tosca.RequirementAssignment)
			reqMap["local_storage"] = req

			node.Requirements = append(node.Requirements, reqMap)
			nodePrefix := path.Join(consulutil.DeploymentKVPrefix, deploymentID, "topology", "nodes", computeNodeName)
			return storage.GetStore(storageTypes.StoreTypeDeployment).Set(nodePrefix, node)
		}
	}

	return nil
}

/**
This function create a given number of floating IP instances
*/
func createNodeInstances(consulStore consulutil.ConsulStore, numberInstances uint32, deploymentID, nodeName string) {

	nodePath := path.Join(consulutil.DeploymentKVPrefix, deploymentID, "topology", "nodes", nodeName)

	consulStore.StoreConsulKeyAsString(path.Join(nodePath, "nbInstances"), strconv.FormatUint(uint64(numberInstances), 10))

	for i := uint32(0); i < numberInstances; i++ {
		instanceName := strconv.FormatUint(uint64(i), 10)
		createNodeInstance(consulStore, deploymentID, nodeName, instanceName)
	}
}

// createInstancesForNodes checks if the given nodes are hosted on a Scalable node,
// stores the number of required instances and sets the instance's status to INITIAL
func createMissingBlockStorageForNodes(ctx context.Context, consulStore consulutil.ConsulStore, deploymentID string, nodeNames []string) error {

	for _, nodeName := range nodeNames {
		requirements, err := GetRequirementsByTypeForNode(ctx, deploymentID, nodeName, "local_storage")
		if err != nil {
			return err
		}

		nbInstances, err := GetNbInstancesForNode(ctx, deploymentID, nodeName)
		if err != nil {
			return err
		}

		var bsName []string

		for _, requirement := range requirements {
			if requirement.Capability != "" {
				bsName = append(bsName, requirement.Node)
			}
		}

		for _, name := range bsName {
			createNodeInstances(consulStore, nbInstances, deploymentID, name)
		}
	}

	return nil
}

/**
This function check if a nodes need a block storage, and return the name of BlockStorage node.
*/
func checkBlockStorage(ctx context.Context, deploymentID, nodeName string) (bool, []string, error) {
	requirements, err := GetRequirementsByTypeForNode(ctx, deploymentID, nodeName, "local_storage")
	if err != nil {
		return false, nil, err
	}

	var bsName []string
	for _, requirement := range requirements {
		if requirement.Capability != "" {
			bsName = append(bsName, requirement.Node)
		}

	}

	return true, bsName, nil
}

// enhanceAttributes walk through the topology nodes an for each of them if needed it creates instances attributes notifications
// to allow resolving any attribute when one is updated
func enhanceAttributes(ctx context.Context, deploymentID string, nodes []string) error {
	for _, nodeName := range nodes {
		// retrieve all node attributes
		attributes, err := GetNodeAttributesNames(ctx, deploymentID, nodeName)
		if err != nil {
			return err
		}

		// retrieve all node instances
		instances, err := GetNodeInstancesIds(ctx, deploymentID, nodeName)
		if err != nil {
			return err
		}

		// 1. Add attribute notifications
		// 2. Resolve attributes and publish default values when not nil or empty
		for _, instanceName := range instances {
			for _, attribute := range attributes {
				err := addAttributeNotifications(ctx, deploymentID, nodeName, instanceName, attribute)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}
