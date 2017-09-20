// Copyright (c) 2017 Tigera, Inc. All rights reserved.
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

package containers

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/felix/fv/utils"
	"github.com/projectcalico/libcalico-go/lib/set"
)

type Container struct {
	Name     string
	IP       string
	Hostname string
	runCmd   *exec.Cmd

	binariesMutex sync.Mutex
	binaries      set.Set
}

var containerIdx = 0

var runningContainers = []*Container{}

func (c *Container) Stop() {
	if c == nil {
		log.Info("Stop no-op because nil container")
	} else if c.runCmd == nil {
		log.WithField("container", c.Name).Info("Stop no-op because container is not running")
	} else {
		log.WithField("container", c).Info("Stop")
		utils.Run("docker", "stop", c.Name)
		c.runCmd = nil

		// And now to be really sure that the container is cleaned up.
		utils.RunMayFail("docker", "rm", "-f", c.Name)
	}
}

func Run(namePrefix string, args ...string) (c *Container) {

	// Build unique container name and struct.
	containerIdx++
	c = &Container{Name: fmt.Sprintf("%v-%d-%d-", namePrefix, os.Getpid(), containerIdx)}

	// Start the container.
	log.WithField("container", c.Name).Info("About to run container")
	runArgs := append([]string{"run", "--name", c.Name}, args...)
	c.runCmd = exec.Command("docker", runArgs...)
	err := c.runCmd.Start()
	Expect(err).NotTo(HaveOccurred())

	// It might take a very long time for the container to show as running, if the image needs
	// to be downloaded - e.g. when running on semaphore.
	c.WaitRunning(20 * 60 * time.Second)

	// Remember that this container is now running.
	runningContainers = append(runningContainers, c)

	// Fill in rest of container struct.
	c.IP = c.GetIP()
	c.Hostname = c.GetHostname()
	c.binaries = set.New()
	log.WithField("container", c).Info("Container now running")
	return
}

func (c *Container) DockerInspect(format string) string {
	inspectCmd := exec.Command("docker", "inspect",
		"--format="+format,
		c.Name,
	)
	outputBytes, err := inspectCmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred())
	return string(outputBytes)
}

func (c *Container) GetIP() string {
	output := c.DockerInspect("{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}")
	return strings.TrimSpace(output)
}

func (c *Container) GetHostname() string {
	output := c.DockerInspect("{{.Config.Hostname}}")
	return strings.TrimSpace(output)
}

func (c *Container) WaitRunning(timeout time.Duration) {
	log.Info("Wait for container to be listed in docker ps")
	start := time.Now()
	for {
		cmd := exec.Command("docker", "ps")
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred())
		if strings.Contains(string(out), c.Name) {
			break
		}
		if time.Since(start) > timeout {
			log.WithField("container", c.Name).Panic("Timed out waiting for container to be listed.")
		}
	}
}

func (c *Container) WaitNotRunning(timeout time.Duration) {
	log.Info("Wait for container not to be listed in docker ps")
	start := time.Now()
	for {
		cmd := exec.Command("docker", "ps")
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred())
		if !strings.Contains(string(out), c.Name) {
			break
		}
		if time.Since(start) > timeout {
			log.WithField("container", c.Name).Panic("Timed out waiting for container not to be listed.")
		}
	}
}

var _ = AfterEach(func() {
	for _, c := range runningContainers {
		c.Stop()
	}
	runningContainers = []*Container{}
})

func (c *Container) EnsureBinary(name string) {
	c.binariesMutex.Lock()
	defer c.binariesMutex.Unlock()

	if !c.binaries.Contains(name) {
		exec.Command("docker", "cp", "../bin/"+name, c.Name+":/"+name).Run()
		c.binaries.Add(name)
	}
}

func (c *Container) Exec(cmd ...string) {
	arg := []string{"exec", c.Name}
	arg = append(arg, cmd...)
	utils.Run("docker", arg...)
}

func (c *Container) ExecMayFail(cmd ...string) {
	arg := []string{"exec", c.Name}
	arg = append(arg, cmd...)
	utils.RunMayFail("docker", arg...)
}

func (c *Container) SourceName() string {
	return c.Name
}

func (c *Container) CanConnectTo(ip, port string) bool {

	// Ensure that the container has the 'test-connection' binary.
	c.EnsureBinary("test-connection")

	// Run 'test-connection' to the target.
	connectionCmd := exec.Command("docker", "exec", c.Name,
		"/test-connection", "-", ip, port)
	outPipe, err := connectionCmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	errPipe, err := connectionCmd.StderrPipe()
	Expect(err).NotTo(HaveOccurred())
	err = connectionCmd.Start()
	Expect(err).NotTo(HaveOccurred())

	wOut, err := ioutil.ReadAll(outPipe)
	Expect(err).NotTo(HaveOccurred())
	wErr, err := ioutil.ReadAll(errPipe)
	Expect(err).NotTo(HaveOccurred())
	err = connectionCmd.Wait()

	log.WithFields(log.Fields{
		"stdout": string(wOut),
		"stderr": string(wErr)}).WithError(err).Info("Connection test")

	return err == nil
}
