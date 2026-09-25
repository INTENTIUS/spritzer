package runtime

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Kubernetes runs each sprite as a pod named sprite-<name> in one namespace,
// with the in-cluster service account. An init container from spritzer's own
// image copies the agent into an emptyDir mounted at /.sprite/bin; the sprite
// container runs `spritzer agent` from there. shareProcessNamespace makes the
// pause container PID 1, which reaps orphans.
//
// The service account needs, in that namespace: pods create/get/list/delete
// and pods/exec create/get.
type Kubernetes struct {
	cfg   Config
	api   string
	ns    string
	token string
	hc    *http.Client
	tlsc  *tls.Config
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// NewKubernetes reads the in-cluster config. namespace overrides the service
// account's own.
func NewKubernetes(cfg Config, namespace string) (*Kubernetes, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST/PORT are unset")
	}
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, fmt.Errorf("service account token: %w", err)
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account CA: no certificates")
	}
	if namespace == "" {
		b, err := os.ReadFile(saDir + "/namespace")
		if err != nil {
			return nil, fmt.Errorf("namespace: %w", err)
		}
		namespace = strings.TrimSpace(string(b))
	}
	tlsc := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &Kubernetes{
		cfg:   cfg,
		api:   "https://" + net.JoinHostPort(host, port),
		ns:    namespace,
		token: strings.TrimSpace(string(token)),
		tlsc:  tlsc,
		hc:    &http.Client{Transport: &http.Transport{TLSClientConfig: tlsc}},
	}, nil
}

// Kind implements Backend.
func (k *Kubernetes) Kind() string { return "kubernetes" }

// Namespace is where sprite pods live.
func (k *Kubernetes) Namespace() string { return k.ns }

func podName(name string) string { return "sprite-" + name }

func (k *Kubernetes) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.api+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("kubernetes %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var st struct{ Message string }
		if json.Unmarshal(b, &st) == nil && st.Message != "" {
			return resp.StatusCode, fmt.Errorf("kubernetes %s %s: %s", method, path, st.Message)
		}
		return resp.StatusCode, fmt.Errorf("kubernetes %s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func (k *Kubernetes) podPath(name string) string {
	return "/api/v1/namespaces/" + k.ns + "/pods/" + podName(name)
}

func (k *Kubernetes) podSpec(name string) map[string]any {
	mount := []map[string]any{{"name": "sprite-bin", "mountPath": BinDir}}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":   podName(name),
			"labels": map[string]string{LabelManagedBy: ManagedBy, LabelSprite: name},
		},
		"spec": map[string]any{
			"hostname":                      name,
			"shareProcessNamespace":         true,
			"automountServiceAccountToken":  false,
			"enableServiceLinks":            false,
			"terminationGracePeriodSeconds": 5,
			"restartPolicy":                 "Always",
			"initContainers": []map[string]any{{
				"name":            "agent-install",
				"image":           k.cfg.AgentImage,
				"imagePullPolicy": "IfNotPresent",
				"args":            []string{"agent-install", BinDir},
				"volumeMounts":    mount,
			}},
			"containers": []map[string]any{{
				"name":            "sprite",
				"image":           k.cfg.SpriteImage,
				"imagePullPolicy": "IfNotPresent",
				"command":         []string{AgentBinary},
				"args":            []string{"agent"},
				"ports":           []map[string]any{{"name": "http", "containerPort": 8080}},
				"volumeMounts":    mount,
			}},
			"volumes": []map[string]any{{"name": "sprite-bin", "emptyDir": map[string]any{}}},
		},
	}
}

type podStatus struct {
	Metadata struct {
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
			State struct {
				Waiting *struct{ Reason, Message string } `json:"waiting"`
			} `json:"state"`
		} `json:"containerStatuses"`
		InitContainerStatuses []struct {
			State struct {
				Waiting    *struct{ Reason, Message string } `json:"waiting"`
				Terminated *struct {
					ExitCode int    `json:"exitCode"`
					Reason   string `json:"reason"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"initContainerStatuses"`
	} `json:"status"`
}

// Create implements Backend.
func (k *Kubernetes) Create(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, k.cfg.CreateTimeout)
	defer cancel()
	st, err := k.call(ctx, http.MethodPost, "/api/v1/namespaces/"+k.ns+"/pods", k.podSpec(name), nil)
	if err != nil && st != http.StatusConflict {
		return err
	}
	// Wait for the sprite container to run, reporting a pull or crash that
	// will not resolve itself instead of timing out on it.
	for {
		var pod podStatus
		if _, err := k.call(ctx, http.MethodGet, k.podPath(name), nil, &pod); err != nil {
			return err
		}
		if pod.Metadata.DeletionTimestamp != nil {
			return fmt.Errorf("sprite %s is being deleted", name)
		}
		running := false
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "sprite" && cs.Ready {
				running = true
			}
			if w := cs.State.Waiting; w != nil && isFatalWait(w.Reason) {
				return fmt.Errorf("sprite %s: %s: %s", name, w.Reason, w.Message)
			}
		}
		for _, cs := range pod.Status.InitContainerStatuses {
			if w := cs.State.Waiting; w != nil && isFatalWait(w.Reason) {
				return fmt.Errorf("sprite %s agent-install: %s: %s", name, w.Reason, w.Message)
			}
		}
		if running && pod.Status.Phase == "Running" {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sprite %s: pod not running after %s (phase %q)", name, k.cfg.CreateTimeout, pod.Status.Phase)
		case <-time.After(500 * time.Millisecond):
		}
	}
	return waitForAgent(ctx, k, name)
}

func isFatalWait(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CrashLoopBackOff":
		return true
	}
	return false
}

// Delete implements Backend, and returns once the pod is gone.
func (k *Kubernetes) Delete(ctx context.Context, name string) error {
	st, err := k.call(ctx, http.MethodDelete, k.podPath(name)+"?gracePeriodSeconds=0", nil, nil)
	if st == http.StatusNotFound {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		st, _ := k.call(ctx, http.MethodGet, k.podPath(name), nil, nil)
		if st == http.StatusNotFound {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sprite %s: pod still present after delete", name)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// List implements Backend.
func (k *Kubernetes) List(ctx context.Context) ([]string, error) {
	var pods struct {
		Items []struct {
			Metadata struct {
				Labels            map[string]string `json:"labels"`
				DeletionTimestamp *string           `json:"deletionTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	sel := url.QueryEscape(LabelManagedBy + "=" + ManagedBy + "," + LabelSprite)
	if _, err := k.call(ctx, http.MethodGet, "/api/v1/namespaces/"+k.ns+"/pods?labelSelector="+sel, nil, &pods); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		if n := p.Metadata.Labels[LabelSprite]; n != "" && p.Metadata.DeletionTimestamp == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// The exec channels of the Kubernetes streaming protocol.
const (
	kStdin  = 0
	kStdout = 1
	kStderr = 2
	kError  = 3
	kClose  = 255 // v5: close the stream named in the next byte
)

// Exec implements Backend over the pods/exec WebSocket (v5.channel.k8s.io,
// which can close stdin; v4 as a fallback, which cannot).
func (k *Kubernetes) Exec(ctx context.Context, name string, argv []string, stdin bool) (Process, error) {
	q := url.Values{"container": {"sprite"}, "stdout": {"true"}, "stderr": {"true"}, "stdin": {strconv.FormatBool(stdin)}}
	for _, a := range argv {
		q.Add("command", a)
	}
	u := strings.Replace(k.api, "https://", "wss://", 1) + k.podPath(name) + "/exec?" + q.Encode()
	// The stream outlives the request that opened it, so it gets its own context.
	sctx, cancel := context.WithCancel(context.Background())
	dctx, dcancel := context.WithTimeout(ctx, 30*time.Second)
	defer dcancel()
	c, resp, err := websocket.Dial(dctx, u, &websocket.DialOptions{
		HTTPClient:   &http.Client{Transport: &http.Transport{TLSClientConfig: k.tlsc}},
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + k.token}},
		Subprotocols: []string{"v5.channel.k8s.io", "v4.channel.k8s.io"},
	})
	if err != nil {
		cancel()
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("kubernetes exec %s: %w", name, err)
	}
	c.SetReadLimit(64 << 20)
	p := &kubeProc{c: c, ctx: sctx, cancel: cancel, v5: c.Subprotocol() == "v5.channel.k8s.io", done: make(chan struct{})}
	p.outR, p.outW = io.Pipe()
	p.errR, p.errW = io.Pipe()
	go p.read()
	return p, nil
}

type kubeProc struct {
	c          *websocket.Conn
	ctx        context.Context
	cancel     context.CancelFunc
	v5         bool
	outR, errR *io.PipeReader
	outW, errW *io.PipeWriter
	done       chan struct{}
	status     []byte
	wmu        sync.Mutex
	stdinDone  bool
}

func (p *kubeProc) read() {
	defer close(p.done)
	var status bytes.Buffer
	for {
		_, data, err := p.c.Read(p.ctx)
		if err != nil {
			break
		}
		if len(data) < 1 {
			continue
		}
		switch data[0] {
		case kStdout:
			_, _ = p.outW.Write(data[1:])
		case kStderr:
			_, _ = p.errW.Write(data[1:])
		case kError:
			status.Write(data[1:])
		}
	}
	p.status = status.Bytes()
	_ = p.outW.Close()
	_ = p.errW.Close()
}

func (p *kubeProc) Stdin() io.WriteCloser { return kubeStdin{p} }
func (p *kubeProc) Stdout() io.Reader     { return p.outR }
func (p *kubeProc) Stderr() io.Reader     { return p.errR }

type kubeStdin struct{ p *kubeProc }

func (s kubeStdin) Write(b []byte) (int, error) {
	s.p.wmu.Lock()
	defer s.p.wmu.Unlock()
	if s.p.stdinDone {
		return 0, io.ErrClosedPipe
	}
	frame := make([]byte, 0, len(b)+1)
	frame = append(frame, kStdin)
	frame = append(frame, b...)
	if err := s.p.c.Write(s.p.ctx, websocket.MessageBinary, frame); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (s kubeStdin) Close() error {
	s.p.wmu.Lock()
	defer s.p.wmu.Unlock()
	if s.p.stdinDone {
		return nil
	}
	s.p.stdinDone = true
	if !s.p.v5 {
		return nil // v4 has no way to close stdin alone
	}
	return s.p.c.Write(s.p.ctx, websocket.MessageBinary, []byte{kClose, kStdin})
}

// Wait implements Process: the exit code rides the error channel as a Status.
func (p *kubeProc) Wait() (int, error) {
	<-p.done
	if len(bytes.TrimSpace(p.status)) == 0 {
		return -1, errors.New("kubernetes exec: stream closed without a status")
	}
	var st struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
		Details struct {
			Causes []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	if err := json.Unmarshal(p.status, &st); err != nil {
		return -1, fmt.Errorf("kubernetes exec status: %w", err)
	}
	if st.Status == "Success" {
		return 0, nil
	}
	for _, c := range st.Details.Causes {
		if c.Reason == "ExitCode" {
			if n, err := strconv.Atoi(c.Message); err == nil {
				return n, nil
			}
		}
	}
	return -1, fmt.Errorf("kubernetes exec: %s", st.Message)
}

// Close implements Process.
func (p *kubeProc) Close() error {
	_ = p.c.CloseNow()
	p.cancel()
	return nil
}
