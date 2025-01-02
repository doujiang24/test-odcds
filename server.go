package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	clustercfg "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corecfg "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointcfg "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	clustersvc "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/golang/protobuf/ptypes"
	"google.golang.org/protobuf/types/known/structpb"
)

type ODCDS struct {
	l    *log.Logger
	resp chan *discovery.DeltaDiscoveryResponse

	mu       sync.Mutex
	clusters []string
}

func (s *ODCDS) StreamClusters(scs clustersvc.ClusterDiscoveryService_StreamClustersServer) error {
	return errors.New("not implemented")
}

func (s *ODCDS) response(dcs clustersvc.ClusterDiscoveryService_DeltaClustersServer) {
	for {
		select {
		case resp := <-s.resp:
			j, err := json.MarshalIndent(resp, "", "  ")
			if err != nil {
				s.l.Printf("Marshaling response JSON: %v", err)
				continue
			}
			s.l.Printf("Sending response:\n%v", string(j))

			err = dcs.Send(resp)
			if err != nil {
				s.l.Printf("Sending response: %v", err)
				continue
			}

		case <-dcs.Context().Done():
			return
		}
	}
}

func (s *ODCDS) updateClusters(dcs clustersvc.ClusterDiscoveryService_DeltaClustersServer) {
	for {
		// check if dcs.Context().Done() channel is closed
		select {
		case <-dcs.Context().Done():
			return

		default:
			// local now timestamp string
			now := time.Now().Format("2006-01-02 15:04:05")

			s.mu.Lock()
			clusters := slices.Clone(s.clusters)
			s.mu.Unlock()

			for _, r := range clusters {
				// Construct a response.
				resources := []*discovery.Resource{}

				label := r + "-" + now

				cluster, err := ptypes.MarshalAny(makeCluster(r, "127.0.0.1", 8082, label))
				if err != nil {
					s.l.Printf("Marshalling cluster config: %v", err)
					continue
				}

				resources = append(resources, &discovery.Resource{
					Name:     r,
					Resource: cluster,
					Version:  "v1",
					// Ttl:      ptypes.DurationProto(5 * time.Second),
				})

				nonce, err := makeNonce()
				if err != nil {
					s.l.Printf("Making nonce: %v", err)
					continue
				}

				resp := &discovery.DeltaDiscoveryResponse{
					Resources:         resources,
					Nonce:             nonce,
					TypeUrl:           "type.googleapis.com/envoy.config.cluster.v3.Cluster",
					SystemVersionInfo: "foo",
				}

				time.Sleep(10 * time.Millisecond)

				s.resp <- resp
			}

			time.Sleep(30 * time.Second)
		}
	}
}

func (s *ODCDS) DeltaClusters(dcs clustersvc.ClusterDiscoveryService_DeltaClustersServer) error {
	// TODO: Handle concurrent requests.
	// TODO: Handle Envoy disconnections properly.

	go s.response(dcs)

	go s.updateClusters(dcs)

	for {
		req, err := dcs.Recv()
		if err != nil {
			s.l.Printf("Receiving request: %v", err)
			continue
		}

		j, err := json.MarshalIndent(req, "", "  ")
		if err != nil {
			s.l.Printf("Marshaling request JSON: %v", err)
			continue
		}
		s.l.Printf("Got request:\n%s\n", string(j))

		if req.ResponseNonce != "" {
			// Request is an ACK of a previous response - no need to return a cluster.
			s.l.Printf("Got an ACK with nonce %s", req.ResponseNonce)
			continue
		}

		// Construct a response.
		resources := []*discovery.Resource{}
		for _, r := range req.ResourceNamesSubscribe {
			if r == "" {
				s.l.Println("Skipping empty resource name")
				continue
			}

			if r == "foo" {
				s.l.Printf("Got invalid cluster name foo")
				return errors.New("invalid cluster name foo")
			}

			label := r + "-init"

			cluster, err := ptypes.MarshalAny(makeCluster(r, "127.0.0.1", 8081, label))
			if err != nil {
				s.l.Printf("Marshalling cluster config: %v", err)
				continue
			}

			resources = append(resources, &discovery.Resource{
				Name:     r,
				Resource: cluster,
				Version:  "v1",
				// Ttl:      ptypes.DurationProto(5 * time.Second),
			})

			s.mu.Lock()
			s.clusters = append(s.clusters, r)
			s.mu.Unlock()
		}

		nonce, err := makeNonce()
		if err != nil {
			s.l.Printf("Making nonce: %v", err)
			continue
		}

		resp := &discovery.DeltaDiscoveryResponse{
			Resources: resources,
			Nonce:     nonce,
			TypeUrl:   "type.googleapis.com/envoy.config.cluster.v3.Cluster",
			// SystemVersionInfo: "foo",
		}

		s.resp <- resp
	}
}

func (s *ODCDS) FetchClusters(context.Context, *discovery.DiscoveryRequest) (*discovery.DiscoveryResponse, error) {
	return nil, errors.New("not implemented")
}

func NewServer(l *log.Logger) *ODCDS {
	return &ODCDS{
		l:    l,
		resp: make(chan *discovery.DeltaDiscoveryResponse),
	}
}

func makeNonce() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func makeCluster(name string, host string, port uint32, label string) *clustercfg.Cluster {
	return &clustercfg.Cluster{
		Name:           name,
		ConnectTimeout: ptypes.DurationProto(2 * time.Second),
		LoadAssignment: &endpointcfg.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointcfg.LocalityLbEndpoints{
				{
					LbEndpoints: []*endpointcfg.LbEndpoint{
						{
							HostIdentifier: &endpointcfg.LbEndpoint_Endpoint{
								Endpoint: &endpointcfg.Endpoint{
									Address: &corecfg.Address{
										Address: &corecfg.Address_SocketAddress{
											SocketAddress: &corecfg.SocketAddress{
												Protocol: corecfg.SocketAddress_TCP,
												Address:  host,
												PortSpecifier: &corecfg.SocketAddress_PortValue{
													PortValue: port,
												},
											},
										},
									},
								},
							},
							Metadata: &corecfg.Metadata{
								FilterMetadata: map[string]*structpb.Struct{
									"envoy.lb": {
										Fields: map[string]*structpb.Value{
											"address": structpb.NewStringValue(label),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
