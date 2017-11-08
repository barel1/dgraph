/*
 * Copyright (C) 2017 Dgraph Labs, Inc. and Contributors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package zero

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/dgraph-io/badger"
	bopts "github.com/dgraph-io/badger/options"
	"github.com/dgraph-io/dgraph/conn"
	"github.com/dgraph-io/dgraph/protos"
	"github.com/dgraph-io/dgraph/raftwal"
	"github.com/dgraph-io/dgraph/x"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/spf13/cobra"
)

type options struct {
	bindall     bool
	myAddr      string
	port        int
	nodeId      uint64
	numReplicas int
	peer        string
	w           string
	lease       int64
}

var opts options

func init() {
	flag := ZeroCmd.Flags()
	flag.BoolVar(&opts.bindall, "bindall", false,
		"Use 0.0.0.0 instead of localhost to bind to all addresses on local machine.")
	flag.StringVar(&opts.myAddr, "my", "",
		"addr:port of this server, so other Dgraph servers can talk to this.")
	flag.IntVar(&opts.port, "port", 8888, "Port to run Dgraph zero on.")
	flag.Uint64Var(&opts.nodeId, "idx", 1, "Unique node index for this server.")
	flag.IntVar(&opts.numReplicas, "replicas", 1, "How many replicas to run per data shard."+
		" The count includes the original shard.")
	flag.StringVar(&opts.peer, "peer", "", "Address of another dgraphzero server.")
	flag.StringVar(&opts.w, "w", "w", "Directory storing WAL.")
	flag.Int64Var(&opts.lease, "lease", 0,
		"Sets a new lease. Ignored if less than the existing lease.")
}

func setupListener(addr string, port int) (listener net.Listener, err error) {
	laddr := fmt.Sprintf("%s:%d", addr, port)
	fmt.Printf("Setting up listener at: %v\n", laddr)
	return net.Listen("tcp", laddr)
}

type state struct {
	node *node
	rs   *conn.RaftServer
	zero *Server
}

func (st *state) serveGRPC(l net.Listener, wg *sync.WaitGroup) {
	s := grpc.NewServer(
		grpc.MaxRecvMsgSize(x.GrpcMaxSize),
		grpc.MaxSendMsgSize(x.GrpcMaxSize),
		grpc.MaxConcurrentStreams(1000))

	if len(opts.myAddr) == 0 {
		opts.myAddr = fmt.Sprintf("localhost:%d", opts.port)
	}
	rc := protos.RaftContext{Id: opts.nodeId, Addr: opts.myAddr, Group: 0}
	m := conn.NewNode(&rc)
	st.rs = &conn.RaftServer{Node: m}

	st.node = &node{Node: m, ctx: context.Background()}
	st.zero = &Server{NumReplicas: opts.numReplicas, Node: st.node}
	st.zero.Init()
	st.node.server = st.zero

	protos.RegisterZeroServer(s, st.zero)
	protos.RegisterRaftServer(s, st.rs)

	go func() {
		defer wg.Done()
		err := s.Serve(l)
		log.Printf("gRpc server stopped : %s", err.Error())
		s.GracefulStop()
	}()
}

func (st *state) serveHTTP(l net.Listener, wg *sync.WaitGroup) {
	srv := &http.Server{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 600 * time.Second,
		IdleTimeout:  2 * time.Minute,
	}

	go func() {
		defer wg.Done()
		err := srv.Serve(l)
		log.Printf("Stopped taking more http(s) requests. Err: %s", err.Error())
		ctx, cancel := context.WithTimeout(context.Background(), 630*time.Second)
		defer cancel()
		err = srv.Shutdown(ctx)
		log.Printf("All http(s) requests finished.")
		if err != nil {
			log.Printf("Http(s) shutdown err: %v", err.Error())
		}
	}()
}

func (st *state) getState(w http.ResponseWriter, r *http.Request) {
	x.AddCorsHeaders(w)
	w.Header().Set("Content-Type", "application/json")

	mstate := st.zero.membershipState()
	if mstate == nil {
		x.SetStatus(w, x.ErrorNoData, "No membership state found.")
		return
	}

	m := jsonpb.Marshaler{}
	if err := m.Marshal(w, mstate); err != nil {
		x.SetStatus(w, x.ErrorNoData, err.Error())
		return
	}
}

var ZeroCmd = &cobra.Command{
	Use:   "zero",
	Short: "Run Dgraph zero server",
	Run: func(cmd *cobra.Command, args []string) {
		run()
	},
}

func run() {
	grpc.EnableTracing = false

	addr := "localhost"
	if opts.bindall {
		addr = "0.0.0.0"
	}

	grpcListener, err := setupListener(addr, opts.port)
	if err != nil {
		log.Fatal(err)
	}
	httpListener, err := setupListener(addr, opts.port+1)
	if err != nil {
		log.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	// Initialize the servers.
	var st state
	st.serveGRPC(grpcListener, &wg)
	st.serveHTTP(httpListener, &wg)

	http.HandleFunc("/state", st.getState)

	// Open raft write-ahead log and initialize raft node.
	x.Checkf(os.MkdirAll(opts.w, 0700), "Error while creating WAL dir.")
	kvOpt := badger.DefaultOptions
	kvOpt.SyncWrites = true
	kvOpt.Dir = opts.w
	kvOpt.ValueDir = opts.w
	kvOpt.TableLoadingMode = bopts.MemoryMap
	kv, err := badger.OpenManaged(kvOpt)
	x.Checkf(err, "Error while opening WAL store")
	defer kv.Close()
	wal := raftwal.Init(kv, opts.nodeId)
	x.Check(st.node.initAndStartNode(wal))

	sdCh := make(chan os.Signal, 1)
	signal.Notify(sdCh, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	if opts.lease != 0 {
		x.Check(st.node.proposeAndWait(context.Background(), &protos.ZeroProposal{
			MaxLeaseId: uint64(opts.lease),
		}))
	}

	go func() {
		defer wg.Done()
		<-sdCh
		fmt.Println("Shutting down...")
		// Close doesn't close already opened connections.
		httpListener.Close()
		grpcListener.Close()
		st.zero.shutDownCh <- struct{}{}
	}()

	fmt.Println("Running Dgraph zero...")
	wg.Wait()
	fmt.Println("All done.")
}