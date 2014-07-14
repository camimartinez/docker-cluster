// Copyright 2014 docker-cluster authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package cluster provides types and functions for management of Docker
// clusters, scheduling container operations among hosts running Docker
// (nodes).
package cluster

import (
	"errors"
	"github.com/fsouza/go-dockerclient"
	"net/http"
	"reflect"
	"sync"
)

var (
	ErrDuplicatedNodeAddress = errors.New("Node address shouldn't repeat")

	errStorageMandatory = errors.New("Storage parameter is mandatory")
)

// ContainerStorage provides methods to store and retrieve information about
// the relation between the node and the container. It can be easily
// represented as a key-value storage.
//
// The relevant information is: in which host the given container is running?
type ContainerStorage interface {
	StoreContainer(container, host string) error
	RetrieveContainer(container string) (host string, err error)
	RemoveContainer(container string) error
}

// ImageStorage works like ContainerStorage, but stores information about
// images and hosts.
type ImageStorage interface {
	StoreImage(image, host string) error
	RetrieveImage(image string) (host []string, err error)
	RemoveImage(image string) error
}

type NodeStorage interface {
	StoreNode(node Node) error
	RetrieveNodesByMetadata(metadata map[string]string) ([]Node, error)
	RetrieveNodes() ([]Node, error)
	RemoveNode(address string) error
}

type Storage interface {
	ContainerStorage
	ImageStorage
	NodeStorage
}

// Node represents a host running Docker. Each node has an ID and an address
// (in the form <scheme>://<host>:<port>/).
type Node struct {
	Address  string
	Metadata map[string]string
}

// Cluster is the basic type of the package. It manages internal nodes, and
// provide methods for interaction with those nodes, like CreateContainer,
// which creates a container in one node of the cluster.
type Cluster struct {
	scheduler Scheduler
	stor      Storage
}

// New creates a new Cluster, initially composed by the given nodes.
//
// The scheduler parameter defines the scheduling strategy. It defaults
// to round robin if nil.
// The storage parameter is the storage the cluster instance will use.
func New(scheduler Scheduler, storage Storage, nodes ...Node) (*Cluster, error) {
	var (
		c   Cluster
		err error
	)
	if storage == nil {
		return nil, errStorageMandatory
	}
	c.stor = storage
	c.scheduler = scheduler
	if scheduler == nil {
		c.scheduler = &roundRobin{lastUsed: -1}
	}
	if len(nodes) > 0 {
		for _, n := range nodes {
			err = c.Register(n.Address, n.Metadata)
			if err != nil {
				return &c, err
			}
		}
	}
	return &c, err
}

// Register adds new nodes to the cluster.
func (c *Cluster) Register(address string, metadata map[string]string) error {
	if address == "" {
		return errors.New("Invalid address")
	}
	node := Node{
		Address:  address,
		Metadata: metadata,
	}
	return c.storage().StoreNode(node)
}

// Unregister removes nodes from the cluster.
func (c *Cluster) Unregister(address string) error {
	return c.storage().RemoveNode(address)
}

func (c *Cluster) Nodes() ([]Node, error) {
	return c.storage().RetrieveNodes()
}

func (c *Cluster) NodesForMetadata(metadata map[string]string) ([]Node, error) {
	return c.storage().RetrieveNodesByMetadata(metadata)
}

func (c *Cluster) storage() Storage {
	return c.stor
}

type nodeFunc func(node) (interface{}, error)

func (c *Cluster) runOnNodes(fn nodeFunc, errNotFound error, wait bool, nodeAddresses ...string) (interface{}, error) {
	nodes, err := c.Nodes()
	if err != nil {
		return nil, err
	}
	if len(nodeAddresses) > 0 {
		nodes = c.filterNodes(nodes, nodeAddresses)
	}
	var wg sync.WaitGroup
	finish := make(chan int8, len(nodes))
	errChan := make(chan error, len(nodes))
	result := make(chan interface{}, len(nodes))
	for _, n := range nodes {
		wg.Add(1)
		client, _ := docker.NewClient(n.Address)
		go func(n node) {
			defer wg.Done()
			value, err := fn(n)
			if err == nil {
				result <- value
			} else if e, ok := err.(*docker.Error); ok && e.Status == http.StatusNotFound {
				return
			} else if !reflect.DeepEqual(err, errNotFound) {
				errChan <- err
			}
		}(node{addr: n.Address, Client: client})
	}
	if wait {
		wg.Wait()
		select {
		case value := <-result:
			return value, nil
		case err := <-errChan:
			return nil, err
		default:
			return nil, errNotFound
		}
	}
	go func() {
		wg.Wait()
		close(finish)
	}()
	select {
	case value := <-result:
		return value, nil
	case err := <-errChan:
		return nil, err
	case <-finish:
		select {
		case value := <-result:
			return value, nil
		default:
			return nil, errNotFound
		}
	}
}

func (c *Cluster) filterNodes(nodes []Node, addresses []string) []Node {
	filteredNodes := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		for _, addr := range addresses {
			if node.Address == addr {
				filteredNodes = append(filteredNodes, node)
				break
			}
		}
	}
	return filteredNodes
}

func (c *Cluster) getNode(retrieveFn func(Storage) (string, error)) (node, error) {
	var n node
	storage := c.storage()
	address, err := retrieveFn(storage)
	if err != nil {
		return n, err
	}
	client, err := docker.NewClient(address)
	if err != nil {
		return n, err
	}
	return node{addr: address, Client: client}, nil
}
