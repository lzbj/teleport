/*
Copyright 2016-2018 Gravitational, Inc.

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

package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/testauthority"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	kubeutils "github.com/gravitational/teleport/lib/kube/utils"
	"github.com/gravitational/teleport/lib/service"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
	"gopkg.in/check.v1"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	streamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
)

var _ = check.Suite(&KubeSuite{})

type KubeSuite struct {
	*kubernetes.Clientset
	kubeConfig *rest.Config
	ports      utils.PortList
	me         *user.User
	// priv/pub pair to avoid re-generating it
	priv []byte
	pub  []byte

	// kubeCACert is a certificate of a kubernetes certificate authority
	kubeCACert []byte
	// kubeCACertPath is a temp file to hold CA certificate authority contents
	kubeCACertPath string
	// kubeAPIHost is a host:port with kubernetes API server
	kubeAPIHost string
}

func (s *KubeSuite) SetUpSuite(c *check.C) {
	var err error
	utils.InitLoggerForTests()
	SetTestTimeouts(time.Millisecond * time.Duration(100))

	s.priv, s.pub, err = testauthority.New().GenerateKeyPair("")
	c.Assert(err, check.IsNil)

	s.ports, err = utils.GetFreeTCPPorts(AllocatePortsNum, utils.PortStartingNumber+AllocatePortsNum+1)
	if err != nil {
		c.Fatal(err)
	}
	s.me, err = user.Current()
	c.Assert(err, check.IsNil)

	// close & re-open stdin because 'go test' runs with os.stdin connected to /dev/null
	stdin, err := os.Open("/dev/tty")
	if err != nil {
		os.Stdin.Close()
		os.Stdin = stdin
	}

	testEnabled := os.Getenv(teleport.KubeRunTests)
	if ok, _ := strconv.ParseBool(testEnabled); !ok {
		c.Skip("Skipping Kubernetes test suite.")
	}

	kubeConfigPath := os.Getenv("KUBECONFIG")
	s.Clientset, s.kubeConfig, err = kubeutils.GetKubeClient(kubeConfigPath)
	c.Assert(err, check.IsNil)

	u, err := url.Parse(s.kubeConfig.Host)
	c.Assert(err, check.IsNil)
	s.kubeAPIHost = u.Host
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		s.kubeAPIHost = fmt.Sprintf("%v:443", u.Host)
	}

	ns := newNamespace(testNamespace)
	_, err = s.Core().Namespaces().Create(ns)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			c.Fatalf("Failed to create namespace: %v.", err)
		}
	}

	// fetch certificate authority cert, by grabbing it
	// from the DNS app pod that is always running
	pods, err := s.Core().Pods(kubeSystemNamespace).List(metav1.ListOptions{
		LabelSelector: kubeDNSLabels.AsSelector().String(),
	})
	c.Assert(err, check.IsNil)
	if len(pods.Items) == 0 {
		c.Fatalf("Failed to find kube-dns pods.")
	}
	log.Infof("Found %v pods", len(pods.Items))
	pod := pods.Items[0]

	out := &bytes.Buffer{}
	err = kubeExec(s.kubeConfig, kubeExecArgs{
		podName:      pod.Name,
		podNamespace: pod.Namespace,
		container:    kubeDNSContainer,
		command:      []string{"/bin/cat", teleport.KubeCAPath},
		stdout:       out,
	})
	c.Assert(err, check.IsNil)
	s.kubeCACert = out.Bytes()
	s.kubeCACertPath = filepath.Join(c.MkDir(), "ca.pem")
	err = ioutil.WriteFile(s.kubeCACertPath, s.kubeCACert, 0444)
	c.Assert(err, check.IsNil)
}

const (
	kubeSystemNamespace = "kube-system"
	kubeDNSContainer    = "kubedns"
)

var kubeDNSLabels = labels.Set{"k8s-app": "kube-dns"}

func (s *KubeSuite) TearDownSuite(c *check.C) {
	var err error
	// restore os.Stdin to its original condition: connected to /dev/null
	os.Stdin.Close()
	os.Stdin, err = os.Open("/dev/null")
	c.Assert(err, check.IsNil)
}

// TestKubeExec tests kubernetes Exec command set
func (s *KubeSuite) TestKubeExec(c *check.C) {
	tconf := s.teleKubeConfig(Host)

	t := NewInstance(InstanceConfig{
		ClusterName: Site,
		HostID:      HostID,
		NodeName:    Host,
		Ports:       s.ports.PopIntSlice(5),
		Priv:        s.priv,
		Pub:         s.pub,
	})

	username := s.me.Username
	role, err := services.NewRole("kubemaster", services.RoleSpecV3{
		Allow: services.RoleConditions{
			Logins:     []string{username},
			KubeGroups: []string{teleport.KubeSystemMasters},
		},
	})
	t.AddUserWithRole(username, role)

	err = t.CreateEx(nil, tconf)
	c.Assert(err, check.IsNil)

	err = t.Start()
	c.Assert(err, check.IsNil)
	defer t.Stop(true)

	// set up kube configuration using proxy
	proxyClient, proxyClientConfig, err := kubeProxyClient(t, username)
	c.Assert(err, check.IsNil)

	// try get request to fetch available pods
	pods, err := proxyClient.Core().Pods(kubeSystemNamespace).List(metav1.ListOptions{
		LabelSelector: kubeDNSLabels.AsSelector().String(),
	})
	c.Assert(len(pods.Items), check.Not(check.Equals), int(0))

	// Exec through proxy and collect output
	pod := pods.Items[0]

	out := &bytes.Buffer{}
	err = kubeExec(proxyClientConfig, kubeExecArgs{
		podName:      pod.Name,
		podNamespace: pod.Namespace,
		container:    kubeDNSContainer,
		command:      []string{"/bin/cat", teleport.KubeCAPath},
		stdout:       out,
	})
	c.Assert(err, check.IsNil)
	// this is the same certificate authority file fetched
	// during suite setup process, so the output should be identical)
	data := out.Bytes()
	c.Assert(string(data), check.Equals, string(s.kubeCACert))

	// interactive command, allocate pty
	term := NewTerminal(250)
	// lets type "echo hi" followed by "enter" and then "exit" + "enter":
	term.Type("\aecho hi\n\r\aexit\n\r\a")

	out = &bytes.Buffer{}
	err = kubeExec(proxyClientConfig, kubeExecArgs{
		podName:      pod.Name,
		podNamespace: pod.Namespace,
		container:    kubeDNSContainer,
		command:      []string{"/bin/sh"},
		stdout:       out,
		tty:          true,
		stdin:        &term,
	})
	c.Assert(err, check.IsNil)

	// verify the session stream output
	sessionStream := out.String()
	comment := check.Commentf("%q", sessionStream)
	c.Assert(strings.Contains(sessionStream, "echo hi"), check.Equals, true, comment)
	c.Assert(strings.Contains(sessionStream, "exit"), check.Equals, true, comment)

	// verify traffic capture and upload, wait for the upload to hit
	var sessionID string
	timeoutC := time.After(10 * time.Second)
loop:
	for {
		select {
		case event := <-t.UploadEventsC:
			sessionID = event.SessionID
			break loop
		case <-timeoutC:
			c.Fatalf("Timeout waiting for upload of session to complete")
		}
	}

	// read back the entire session and verify that it matches the stated output
	capturedStream, err := t.Process.GetAuthServer().GetSessionChunk(defaults.Namespace, session.ID(sessionID), 0, events.MaxChunkBytes)
	c.Assert(err, check.IsNil)

	c.Assert(string(capturedStream), check.Equals, sessionStream)

}

// TestKubePortForward tests kubernetes port forwarding
func (s *KubeSuite) TestKubePortForward(c *check.C) {
	tconf := s.teleKubeConfig(Host)

	t := NewInstance(InstanceConfig{
		ClusterName: Site,
		HostID:      HostID,
		NodeName:    Host,
		Ports:       s.ports.PopIntSlice(5),
		Priv:        s.priv,
		Pub:         s.pub,
	})

	username := s.me.Username
	role, err := services.NewRole("kubemaster", services.RoleSpecV3{
		Allow: services.RoleConditions{
			Logins:     []string{username},
			KubeGroups: []string{teleport.KubeSystemMasters},
		},
	})
	t.AddUserWithRole(username, role)

	err = t.CreateEx(nil, tconf)
	c.Assert(err, check.IsNil)

	err = t.Start()
	c.Assert(err, check.IsNil)
	defer t.Stop(true)

	// set up kube configuration using proxy
	_, proxyClientConfig, err := kubeProxyClient(t, username)
	c.Assert(err, check.IsNil)

	// pick the first kube-dns pod and run port forwarding on it
	pods, err := s.Core().Pods(kubeSystemNamespace).List(metav1.ListOptions{
		LabelSelector: kubeDNSLabels.AsSelector().String(),
	})
	c.Assert(len(pods.Items), check.Not(check.Equals), int(0))

	pod := pods.Items[0]

	// forward local port to target port 53 of the dnsmasq container
	localPort := s.ports.Pop()

	forwarder, err := newPortForwarder(proxyClientConfig, kubePortForwardArgs{
		ports:        []string{fmt.Sprintf("%v:53", localPort)},
		podName:      pod.Name,
		podNamespace: pod.Namespace,
	})
	c.Assert(err, check.IsNil)
	go func() {
		err := forwarder.ForwardPorts()
		if err != nil {
			c.Fatalf("Forward ports exited with error: %v.", err)
		}
	}()

	select {
	case <-time.After(5 * time.Second):
		c.Fatalf("Timeout waiting for port forwarding.")
	case <-forwarder.readyC:
	}
	defer close(forwarder.stopC)

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return net.Dial("tcp", fmt.Sprintf("localhost:%v", localPort))
		},
	}
	addr, err := resolver.LookupHost(context.TODO(), "kubernetes.default.svc.cluster.local")
	c.Assert(err, check.IsNil)
	c.Assert(len(addr), check.Not(check.Equals), 0)
}

// teleKubeConfig sets up teleport with kubernetes turned on
func (s *KubeSuite) teleKubeConfig(hostname string) *service.Config {
	tconf := service.MakeDefaultConfig()
	tconf.SSH.Enabled = true
	tconf.Proxy.DisableWebInterface = true
	tconf.PollingPeriod = 500 * time.Millisecond
	tconf.ClientTimeout = time.Second
	tconf.ShutdownTimeout = 2 * tconf.ClientTimeout

	// set kubernetes specific parameters
	tconf.Proxy.KubeListenAddr.Addr = net.JoinHostPort(hostname, s.ports.Pop())
	tconf.Proxy.KubeAPIAddr.Addr = s.kubeAPIHost
	tconf.Auth.KubeCACertPath = s.kubeCACertPath

	return tconf
}

// tlsClientConfig returns TLS configuration for client
func tlsClientConfig(cfg *rest.Config) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(cfg.TLSClientConfig.CertData, cfg.TLSClientConfig.KeyData)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	pool := x509.NewCertPool()
	ok := pool.AppendCertsFromPEM(cfg.TLSClientConfig.CAData)
	if !ok {
		return nil, trace.BadParameter("failed to append certs from PEM")
	}

	tlsConfig := &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	tlsConfig.BuildNameToCertificate()
	return tlsConfig, nil
}

// kubeProxyClient returns kubernetes client using local teleport proxy
func kubeProxyClient(t *TeleInstance, username string) (*kubernetes.Clientset, *rest.Config, error) {
	authServer := t.Process.GetAuthServer()
	clusterName, err := authServer.GetClusterName()
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	ca, err := authServer.GetCertAuthority(services.CertAuthID{
		Type:       services.HostCA,
		DomainName: clusterName.GetClusterName(),
	}, false)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	cert, key, err := auth.GenerateCertificate(authServer,
		auth.TestIdentity{I: auth.LocalUser{Username: username}})
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	tlsClientConfig := rest.TLSClientConfig{
		CAData:   ca.GetTLSKeyPairs()[0].Cert,
		CertData: cert,
		KeyData:  key,
	}
	config := &rest.Config{
		Host:            "https://" + t.Config.Proxy.KubeListenAddr.Addr,
		TLSClientConfig: tlsClientConfig,
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	return client, config, nil
}

const (
	testTimeout = 1 * time.Minute

	testNamespace = "teletest"
)

func newNamespace(name string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

type kubeExecArgs struct {
	podName      string
	podNamespace string
	container    string
	command      []string
	stdout       io.Writer
	stderr       io.Writer
	stdin        io.Reader
	tty          bool
}

func toBoolString(val bool) string {
	return fmt.Sprintf("%t", val)
}

type kubePortForwardArgs struct {
	ports        []string
	podName      string
	podNamespace string
}

type kubePortForwarder struct {
	*portforward.PortForwarder
	stopC  chan struct{}
	readyC chan struct{}
}

func newPortForwarder(kubeConfig *rest.Config, args kubePortForwardArgs) (*kubePortForwarder, error) {
	u, err := url.Parse(kubeConfig.Host)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	u.Scheme = "https"
	u.Path = fmt.Sprintf("/api/v1/namespaces/%v/pods/%v/portforward", args.podNamespace, args.podName)

	// set up port forwarding request
	tlsConfig, err := tlsClientConfig(kubeConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	upgradeRoundTripper := streamspdy.NewSpdyRoundTripper(tlsConfig, true)
	client := &http.Client{
		Transport: upgradeRoundTripper,
	}
	dialer := spdy.NewDialer(upgradeRoundTripper, client, "POST", u)

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	stopC, readyC := make(chan struct{}), make(chan struct{})
	fwd, err := portforward.New(dialer, args.ports, stopC, readyC, out, errOut)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &kubePortForwarder{PortForwarder: fwd, stopC: stopC, readyC: readyC}, nil
}

// kubeExec executes command against kubernetes API server
func kubeExec(kubeConfig *rest.Config, args kubeExecArgs) error {
	query := make(url.Values)
	for _, arg := range args.command {
		query.Add("command", arg)
	}
	if args.stdout != nil {
		query.Set("stdout", "true")
	}
	if args.stdin != nil {
		query.Set("stdin", "true")
	}
	// stderr channel is only set if there is no tty allocated
	// otherwise k8s server gets confused
	if !args.tty && args.stderr == nil {
		args.stderr = ioutil.Discard
	}
	if args.stderr != nil && !args.tty {
		query.Set("stderr", "true")
	}
	if args.tty {
		query.Set("tty", "true")
	}
	query.Set("container", args.container)
	u, err := url.Parse(kubeConfig.Host)
	if err != nil {
		return trace.Wrap(err)
	}
	u.Scheme = "https"
	u.Path = fmt.Sprintf("/api/v1/namespaces/%v/pods/%v/exec", args.podNamespace, args.podName)
	u.RawQuery = query.Encode()
	executor, err := remotecommand.NewSPDYExecutor(kubeConfig, "POST", u)
	if err != nil {
		return trace.Wrap(err)
	}
	opts := remotecommand.StreamOptions{
		Stdin:  args.stdin,
		Stdout: args.stdout,
		Stderr: args.stderr,
		Tty:    args.tty,
	}
	return executor.Stream(opts)
}
