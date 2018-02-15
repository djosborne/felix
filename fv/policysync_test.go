// +build fvtests

// Copyright (c) 2018 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fv

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/colabsaumoh/proto-udsuspver/nodeagentmgmt"
	"github.com/colabsaumoh/proto-udsuspver/protos/mgmtintf_v1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/projectcalico/libcalico-go/lib/options"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/projectcalico/felix/dataplane/mock"
	"github.com/projectcalico/libcalico-go/lib/set"

	"github.com/projectcalico/felix/proto"

	"github.com/projectcalico/felix/fv/containers"
	"github.com/projectcalico/felix/fv/utils"
	"github.com/projectcalico/felix/fv/workload"
	api "github.com/projectcalico/libcalico-go/lib/apis/v3"
	client "github.com/projectcalico/libcalico-go/lib/clientv3"
)

var _ = Context("policy sync API tests", func() {

	var (
		etcd               *containers.Container
		felix              *containers.Felix
		calicoClient       client.Interface
		w                  [3]*workload.Workload
		tempDir            string
		hostMgmtSocketPath string
	)

	BeforeEach(func() {
		// Create a temporary directory to map into the container as /var/run/calico, which
		// is where we tell Felix to put the policy sync management socket.
		var err error
		tempDir, err = ioutil.TempDir("", "felixfv")
		Expect(err).NotTo(HaveOccurred())

		// Configure felix to enable the policy sync API.
		options := containers.DefaultTopologyOptions()
		options.ExtraEnvVars["FELIX_PolicySyncManagementSocketPath"] = "/var/run/calico/policy-mgmt.sock"
		options.ExtraEnvVars["FELIX_PolicySyncWorkloadSocketPathPrefix"] = "/var/run/calico"
		// To enable debug logs, uncomment these lines; watch out for timeouts caused by the
		// resulting slow down!
		// options.ExtraEnvVars["FELIX_DebugDisableLogDropping"] = "true"
		// options.FelixLogSeverity = "debug"
		options.ExtraVolumes[tempDir] = "/var/run/calico"
		felix, etcd, calicoClient = containers.StartSingleNodeEtcdTopology(options)

		// Install a default profile that allows workloads with this profile to talk to each
		// other, in the absence of any Policy.
		defaultProfile := api.NewProfile()
		defaultProfile.Name = "default"
		defaultProfile.Spec.LabelsToApply = map[string]string{"default": ""}
		defaultProfile.Spec.Egress = []api.Rule{{Action: api.Allow}}
		defaultProfile.Spec.Ingress = []api.Rule{{
			Action: api.Allow,
			Source: api.EntityRule{Selector: "default == ''"},
		}}
		_, err = calicoClient.Profiles().Create(utils.Ctx, defaultProfile, utils.NoOptions)
		Expect(err).NotTo(HaveOccurred())

		// Create three workloads, using that profile.
		for ii := range w {
			iiStr := strconv.Itoa(ii)
			w[ii] = workload.Run(felix, "w"+iiStr, "cali1"+iiStr, "10.65.0.1"+iiStr, "8055", "tcp")
			w[ii].WorkloadEndpoint.Spec.Endpoint = "eth0"
			w[ii].WorkloadEndpoint.Spec.Orchestrator = "k8s"
			w[ii].WorkloadEndpoint.Spec.Pod = "fv-pod-" + iiStr
			w[ii].Configure(calicoClient)
		}

		hostMgmtSocketPath = tempDir + "/policy-mgmt.sock"
	})

	AfterEach(func() {
		for ii := range w {
			w[ii].Stop()
		}
		felix.Stop()

		if CurrentGinkgoTestDescription().Failed {
			etcd.Exec("etcdctl", "ls", "--recursive", "/")
		}
		etcd.Stop()
	})

	AfterEach(func() {
		if tempDir != "" {
			err := os.RemoveAll(tempDir)
			Expect(err).NotTo(HaveOccurred(), "Failed to clean up temp dir")
		}
	})

	Context("with an open management socket", func() {
		var (
			mgmtClient *nodeagentmgmt.Client
		)

		BeforeEach(func() {
			Eventually(hostMgmtSocketPath).Should(BeAnExistingFile())

			// Use the fact that anything we exec inside the Felix container runs as root to fix the
			// permissions on the socket so the test process can connect.
			felix.Exec("chmod", "a+rw", "/var/run/calico/policy-mgmt.sock")
			mgmtClient = nodeagentmgmt.ClientUds(hostMgmtSocketPath)
		})

		var (
			hostWlSocketPath, containerWlSocketPath [3]string
		)

		dirNameForWorkload := func(wl *workload.Workload) string {
			return "ps-" + wl.WorkloadEndpoint.Spec.Pod
		}

		createWorkloadDirectory := func(wl *workload.Workload) (string, string) {
			dirName := dirNameForWorkload(wl)
			hostWlDir := tempDir + "/" + dirName
			os.MkdirAll(hostWlDir, 0777)
			return hostWlDir, "/var/run/calico/" + dirName
		}

		sendCreate := func(wl *workload.Workload) (*mgmtintf_v1.Response, error) {
			dirName := dirNameForWorkload(wl)

			resp, err := mgmtClient.WorkloadAdded(&mgmtintf_v1.WorkloadInfo{
				Attrs: &mgmtintf_v1.WorkloadInfo_WorkloadAttributes{
					Uid:       wl.WorkloadEndpoint.Spec.Pod,
					Namespace: "fv",
					Workload:  wl.WorkloadEndpoint.Spec.Pod,
				},
				Workloadpath: dirName,
			})
			log.WithField("response", resp).Info("WorkloadAdded response")
			return resp, err
		}

		Context("after creating a client for each workload", func() {
			BeforeEach(func() {
				for i, wl := range w {
					// Create the workload directory, this would normally be the responsibility of the
					// flex volume driver.
					hostWlDir, containerWlDir := createWorkloadDirectory(wl)
					hostWlSocketPath[i] = hostWlDir + "/policysync.sock"
					containerWlSocketPath[i] = containerWlDir + "/policysync.sock"

					// Tell Felix about the new directory.
					_, err := sendCreate(wl)
					Expect(err).NotTo(HaveOccurred())
				}
			})

			It("felix should create the workload socket", func() {
				for _, p := range hostWlSocketPath {
					Eventually(p).Should(BeAnExistingFile())
				}
			})

			Context("with open workload connections", func() {

				// Then connect to it.
				var (
					wlConn   [3]*grpc.ClientConn
					wlClient [3]proto.PolicySyncClient
					cancel   context.CancelFunc
					ctx      context.Context
					err      error
				)

				createWorkloadConn := func(i int) (*grpc.ClientConn, proto.PolicySyncClient) {
					var opts []grpc.DialOption
					opts = append(opts, grpc.WithInsecure())
					opts = append(opts, grpc.WithDialer(unixDialer))
					var conn *grpc.ClientConn
					conn, err = grpc.Dial(hostWlSocketPath[i], opts...)
					Expect(err).NotTo(HaveOccurred())
					wlClient := proto.NewPolicySyncClient(conn)
					return conn, wlClient
				}

				BeforeEach(func() {
					ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)

					for i := range w {
						// Use the fact that anything we exec inside the Felix container runs as root to fix the
						// permissions on the socket so the test process can connect.
						Eventually(hostWlSocketPath[i]).Should(BeAnExistingFile())
						felix.Exec("chmod", "a+rw", containerWlSocketPath[i])
						wlConn[i], wlClient[i] = createWorkloadConn(i)
					}
				})

				AfterEach(func() {
					if cancel != nil {
						cancel()
					}
				})

				Context("with mock clients syncing", func() {
					var (
						mockWlClient [3]*mockWorkloadClient
						mockWlCancel [3]context.CancelFunc
					)

					BeforeEach(func() {
						for i := range w {
							client := newMockWorkloadClient(fmt.Sprintf("workload-%d", i))
							var wlCtx context.Context
							wlCtx, mockWlCancel[i] = context.WithCancel(ctx)
							client.StartSyncing(wlCtx, wlClient[i])
							mockWlClient[i] = client
						}
					})

					AfterEach(func() {
						log.Info("AfterEach: cancelling main context")
						cancel()
						for _, c := range mockWlClient {
							Eventually(c.Done).Should(BeClosed())
						}
					})

					It("workload 0's client should receive correct updates", func() {
						Eventually(mockWlClient[0].InSync).Should(BeTrue())
						Eventually(mockWlClient[0].ActiveProfiles).Should(Equal(set.From(proto.ProfileID{Name: "default"})))
						// Should only hear about our own workload.
						Eventually(mockWlClient[0].EndpointToPolicyOrder).Should(Equal(
							map[string][]mock.TierInfo{"k8s/fv/fv-pod-0/eth0": {}}))
					})

					It("workload 1's client should receive correct updates", func() {
						Eventually(mockWlClient[1].InSync).Should(BeTrue())
						Eventually(mockWlClient[1].ActiveProfiles).Should(Equal(set.From(proto.ProfileID{Name: "default"})))
						// Should only hear about our own workload.
						Eventually(mockWlClient[1].EndpointToPolicyOrder).Should(Equal(
							map[string][]mock.TierInfo{"k8s/fv/fv-pod-1/eth0": {}}))
					})

					Context("after closing one client's gRPC connection", func() {
						BeforeEach(func() {
							// Sanity check that the connection is up before we close it.
							Eventually(mockWlClient[2].InSync, "10s").Should(BeTrue())

							// Close it and wait for the client to shut down.
							wlConn[2].Close()
							Eventually(mockWlClient[2].Done, "10s").Should(BeClosed())
						})

						doChurn := func(wlIndexes ...int) {
							for i := 0; i < 100; i++ {
								wlIdx := wlIndexes[i%len(wlIndexes)]
								By(fmt.Sprintf("Churn %d; targetting workload %d", i, wlIdx))

								policy := api.NewGlobalNetworkPolicy()
								policy.SetName("policy-0")
								policy.Spec.Selector = w[wlIdx].NameSelector()

								policy, err = calicoClient.GlobalNetworkPolicies().Create(ctx, policy, utils.NoOptions)
								Expect(err).NotTo(HaveOccurred())

								if wlIdx != 2 {
									policyID := proto.PolicyID{Name: "default.policy-0", Tier: "default"}
									Eventually(mockWlClient[wlIdx].ActivePolicies).Should(Equal(set.From(policyID)))
								}

								_, err = calicoClient.GlobalNetworkPolicies().Delete(ctx, "policy-0", options.DeleteOptions{})
								Expect(err).NotTo(HaveOccurred())

								if wlIdx != 2 {
									Eventually(mockWlClient[wlIdx].ActivePolicies).Should(Equal(set.New()))
								}
							}
						}

						It("churn affecting all endpoints should result in expected updates", func() {
							// Send in some churn to ensure that we exhaust any buffers that might let
							// one or two updates through.
							doChurn(0, 1, 2)
						})

						It("churn affecting only active endpoints should result in expected updates", func() {
							// Send in some churn to ensure that we exhaust any buffers that might let
							// one or two updates through.
							doChurn(0, 1)
						})
					})

					Context("after adding a policy that applies to workload 0 only", func() {
						var (
							policy   *api.GlobalNetworkPolicy
							policyID proto.PolicyID
						)

						BeforeEach(func() {
							policy = api.NewGlobalNetworkPolicy()
							policy.SetName("policy-0")
							policy.Spec.Selector = w[0].NameSelector()
							policy.Spec.Ingress = []api.Rule{
								{
									Action: "Allow",
									Source: api.EntityRule{
										Selector: "all()",
									},
								},
							}
							policy.Spec.Egress = []api.Rule{
								{
									Action: "Allow",
								},
							}
							policy, err = calicoClient.GlobalNetworkPolicies().Create(ctx, policy, utils.NoOptions)
							Expect(err).NotTo(HaveOccurred())

							policyID = proto.PolicyID{Name: "default.policy-0", Tier: "default"}
						})

						It("should be sent to workload 0 only", func() {
							Eventually(mockWlClient[0].ActivePolicies).Should(Equal(set.From(
								policyID,
							)))
							Eventually(mockWlClient[0].EndpointToPolicyOrder).Should(Equal(
								map[string][]mock.TierInfo{"k8s/fv/fv-pod-0/eth0": {{
									Name:               "default",
									EgressPolicyNames:  []string{"default.policy-0"},
									IngressPolicyNames: []string{"default.policy-0"},
								}}}))

							Consistently(mockWlClient[1].ActivePolicies).Should(Equal(set.New()))
							Consistently(mockWlClient[2].ActivePolicies).Should(Equal(set.New()))
						})

						It("should handle a deletion", func() {
							// Make sure the initial update makes it through or we might get a
							// false positive.
							Eventually(mockWlClient[0].ActivePolicies).Should(Equal(set.From(policyID)))

							_, err := calicoClient.GlobalNetworkPolicies().Delete(ctx, "policy-0", options.DeleteOptions{})
							Expect(err).NotTo(HaveOccurred())

							Eventually(mockWlClient[0].ActivePolicies).Should(Equal(set.New()))
						})

						It("should handle a change of selector", func() {
							// Make sure the initial update makes it through or we might get a
							// false positive.
							Eventually(mockWlClient[0].ActivePolicies).Should(Equal(set.From(
								policyID,
							)))

							By("Sending through an endpoint update and policy remove")
							policy.Spec.Selector = w[1].NameSelector()
							var err error
							policy, err = calicoClient.GlobalNetworkPolicies().Update(ctx, policy, options.SetOptions{})
							Expect(err).NotTo(HaveOccurred())

							Eventually(mockWlClient[0].EndpointToPolicyOrder).Should(Equal(
								map[string][]mock.TierInfo{"k8s/fv/fv-pod-0/eth0": {}}))
							Eventually(mockWlClient[0].ActivePolicies).Should(Equal(set.New()))

							By("Updating workload 1 to make the policy active")
							Eventually(mockWlClient[1].ActivePolicies).Should(Equal(set.From(policyID)))
							Eventually(mockWlClient[1].EndpointToPolicyOrder).Should(Equal(
								map[string][]mock.TierInfo{"k8s/fv/fv-pod-1/eth0": {{
									Name:               "default",
									EgressPolicyNames:  []string{"default.policy-0"},
									IngressPolicyNames: []string{"default.policy-0"},
								}}}))

							Consistently(mockWlClient[2].ActivePolicies).Should(Equal(set.New()))
						})

						It("should handle a change of profiles", func() {
							// Make sure the initial update makes it through or we might get a
							// false positive.
							defProfID := proto.ProfileID{Name: "default"}
							Eventually(mockWlClient[0].ActiveProfiles).Should(Equal(set.From(
								defProfID,
							)))
							Eventually(mockWlClient[0].EndpointToProfiles).Should(Equal(map[string][]string{
								"k8s/fv/fv-pod-0/eth0": {"default"}}))

							// Send in an endpoint update that adds one profile and deletes another.
							var err error
							w[0].WorkloadEndpoint.Spec.Profiles = []string{"notdefault"}
							w[0].WorkloadEndpoint, err = calicoClient.WorkloadEndpoints().Update(ctx, w[0].WorkloadEndpoint, options.SetOptions{})
							Expect(err).NotTo(HaveOccurred())

							By("Sending through an endpoint update and policy remove/update")
							notDefProfID := proto.ProfileID{Name: "notdefault"}
							Eventually(mockWlClient[0].EndpointToProfiles).Should(Equal(map[string][]string{
								"k8s/fv/fv-pod-0/eth0": {"notdefault"}}))
							Eventually(mockWlClient[0].ActiveProfiles).Should(Equal(set.From(notDefProfID)))

							Consistently(mockWlClient[2].ActiveProfiles).Should(Equal(set.From(defProfID)))
						})
					})
				})

				It("a connection should get closed if a second connection is created", func() {
					// Create first connection manually.
					syncClient, err := wlClient[0].Sync(ctx, &proto.SyncRequest{})
					Expect(err).NotTo(HaveOccurred())
					// Get something from the first connection to make sure it's up.
					_, err = syncClient.Recv()
					Expect(err).NotTo(HaveOccurred())

					// Then create a new mock client.
					client := newMockWorkloadClient("workload-0 second client")
					client.StartSyncing(ctx, wlClient[0])

					// The new client should take over, getting a full sync.
					Eventually(client.InSync).Should(BeTrue())
					Eventually(client.ActiveProfiles).Should(Equal(set.From(proto.ProfileID{Name: "default"})))
					Eventually(client.EndpointToPolicyOrder).Should(Equal(map[string][]mock.TierInfo{"k8s/fv/fv-pod-0/eth0": {}}))

					// The old connection should get killed.
					Eventually(func() error {
						_, err := syncClient.Recv()
						return err
					}).Should(HaveOccurred())

					cancel()
					Eventually(client.Done).Should(BeClosed())
				})

				It("a connection should get closed if the workload is removed", func() {
					client := newMockWorkloadClient("workload-0")
					client.StartSyncing(ctx, wlClient[0])

					// Workload should be sent over the API.
					Eventually(client.EndpointToPolicyOrder).Should(Equal(map[string][]mock.TierInfo{"k8s/fv/fv-pod-0/eth0": {}}))

					// Deleting the workload from the datastore should send a delete on the sync
					// socket and then close the connection.
					w[0].RemoveFromDatastore(calicoClient)
					Eventually(client.EndpointToPolicyOrder).Should(Equal(map[string][]mock.TierInfo{}))

					Eventually(client.Done).Should(BeClosed())
				})
			})
		})
	})
})

func unixDialer(target string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", target, timeout)
}

type mockWorkloadClient struct {
	*mock.MockDataplane
	name string
	Done chan struct{}
}

func newMockWorkloadClient(name string) *mockWorkloadClient {
	return &mockWorkloadClient{
		name:          name,
		MockDataplane: mock.NewMockDataplane(),
		Done:          make(chan struct{}),
	}
}

func (c *mockWorkloadClient) StartSyncing(ctx context.Context, policySyncClient proto.PolicySyncClient) {
	syncClient, err := policySyncClient.Sync(ctx, &proto.SyncRequest{})
	Expect(err).NotTo(HaveOccurred())
	go c.loopReadingFromAPI(ctx, syncClient)
}

func (c *mockWorkloadClient) loopReadingFromAPI(ctx context.Context, syncClient proto.PolicySync_SyncClient) {
	defer GinkgoRecover()
	defer close(c.Done)

	for ctx.Err() == nil {
		msg, err := syncClient.Recv()
		if err != nil {
			log.WithError(err).WithField("workload", c.name).Warn("Recv failed.")
			return
		}
		log.WithField("msg", msg).Info("Received workload message")
		c.OnEvent(reflect.ValueOf(msg.Payload).Elem().Field(0).Interface())
	}
}
