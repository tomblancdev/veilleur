package door

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/tomblancdev/veilleur/internal/config"
)

// SSH runs the forced command over one scoped identity. Host keys are pinned.
type SSH struct {
	user    string
	timeout time.Duration
	signer  ssh.Signer
	hosts   map[string]config.DoorHost
	keys    map[string]ssh.PublicKey
}

// NewSSH loads the private key and pins every declared host key.
func NewSSH(cfg config.Door) (*SSH, error) {
	rawKey, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("door key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(rawKey)
	if err != nil {
		return nil, fmt.Errorf("door key: %w", err)
	}
	s := &SSH{
		user: cfg.User, timeout: cfg.Timeout.D(),
		signer: signer,
		hosts:  map[string]config.DoorHost{}, keys: map[string]ssh.PublicKey{},
	}
	if s.timeout == 0 {
		s.timeout = 60 * time.Second
	}
	if s.user == "" {
		s.user = "root"
	}
	for _, h := range cfg.Hosts {
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(h.HostKey))
		if err != nil {
			return nil, fmt.Errorf("door host %s: host key: %w", h.Node, err)
		}
		s.hosts[h.Node] = h
		s.keys[h.Node] = pk
	}
	return s, nil
}

// Nodes lists the doors we hold, sorted.
func (s *SSH) Nodes() []string {
	out := make([]string, 0, len(s.hosts))
	for n := range s.hosts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// controls lists the 24/7 nodes, sorted: which one is asked first must be
// the same on every pass, or a half-broken door makes the fleet flap.
func (s *SSH) controls() []string {
	var out []string
	for node, h := range s.hosts {
		if h.Control {
			out = append(out, node)
		}
	}
	sort.Strings(out)
	return out
}

func (s *SSH) run(ctx context.Context, node, args string) (Answer, error) {
	h, ok := s.hosts[node]
	if !ok {
		return Answer{}, &ErrUnreachable{Node: node, Err: fmt.Errorf("no door declared")}
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	// A machine that is off does not refuse a connection, it says nothing —
	// so a dial to it burns the whole command timeout. Reaching a live node
	// takes milliseconds on a LAN; give the dial its own short budget and
	// leave the long one for the command actually running.
	dialTimeout := s.timeout / 6
	if dialTimeout < 5*time.Second {
		dialTimeout = 5 * time.Second
	}
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", h.Addr)
	if err != nil {
		return Answer{}, &ErrUnreachable{Node: node, Err: err}
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	key := s.keys[node]
	cc, chans, reqs, err := ssh.NewClientConn(conn, h.Addr, &ssh.ClientConfig{
		User:              s.user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(s.signer)},
		HostKeyCallback:   ssh.FixedHostKey(key),
		HostKeyAlgorithms: []string{key.Type()},
		Timeout:           s.timeout,
	})
	if err != nil {
		conn.Close()
		return Answer{}, &ErrUnreachable{Node: node, Err: err}
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return Answer{}, &ErrUnreachable{Node: node, Err: err}
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	err = sess.Run(args)
	ans := Answer{
		Node:   node,
		Stdout: strings.TrimSpace(out.String()),
		Stderr: strings.TrimSpace(errb.String()),
	}
	if err == nil {
		return ans, nil
	}
	// A non-zero exit is an ANSWER for a question (a signal, a `state`
	// probe): the signal is false. For an ACTION it is a refusal, and Act
	// turns it into an error - here we only carry the exit and the node's own
	// words up, instead of dropping them as this used to.
	var ee *ssh.ExitError
	if errorAs(err, &ee) {
		ans.Exit = ee.ExitStatus()
		return ans, nil
	}
	msg := ans.Stderr
	if msg == "" {
		msg = err.Error()
	}
	return Answer{}, &ErrUnreachable{Node: node, Err: fmt.Errorf("%s", msg)}
}

// anyControl tries each 24/7 node in turn; unreachable is only fatal if they
// all are.
func (s *SSH) anyControl(ctx context.Context, args string) (Answer, error) {
	var last error
	for _, node := range s.controls() {
		ans, err := s.run(ctx, node, args)
		if err == nil {
			return ans, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("no control node declared")
	}
	return Answer{}, last
}

func (s *SSH) dispatch(ctx context.Context, node, args string) (Answer, error) {
	if node == config.AnyControl || node == "" {
		return s.anyControl(ctx, args)
	}
	return s.run(ctx, node, args)
}

// Signal asks a named question.
func (s *SSH) Signal(ctx context.Context, node, name string) (Answer, error) {
	if err := safeName(name); err != nil {
		return Answer{}, err
	}
	return s.dispatch(ctx, node, "signal "+name)
}

// Act runs up | down | state for a target.
//
// `state` is a QUESTION: a non-zero exit means "not up" and is returned as an
// answer. `up` and `down` are ACTIONS: a non-zero exit means the node was
// reached and refused, which is an error carrying the node's own words. They
// shared the "non-zero is an answer" path once, and a `up` that failed was
// therefore recorded as done - the caller waited out its whole up_timeout and
// could only report that nothing had happened.
func (s *SSH) Act(ctx context.Context, node, verb, target string) (Answer, error) {
	if err := safeVerb(verb); err != nil {
		return Answer{}, err
	}
	if err := safeName(target); err != nil {
		return Answer{}, err
	}
	ans, err := s.dispatch(ctx, node, verb+" "+target)
	if err != nil {
		return ans, err
	}
	return judge(verb, target, ans)
}

// errorAs is errors.As without importing errors twice over.
func errorAs(err error, target **ssh.ExitError) bool {
	for err != nil {
		if e, ok := err.(*ssh.ExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
