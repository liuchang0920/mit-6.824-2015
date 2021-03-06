package viewservice

import "net"
import "net/rpc"
import "log"
import "time"
import "sync"
import "fmt"
import "os"
import "sync/atomic"

// for debugging
const Debug = 0
func DPrintf(format string, a ...interface{}) {
	if Debug == 1 {
		log.Printf(format, a...)
	}
}

type ViewServer struct {
	mu       sync.Mutex
	l        net.Listener
	dead     int32 // for testing
	rpccount int32 // for testing
	me       string


	// Your declarations here.
	view     *View                 // current view				
	newv     *View                 // new view
	packed   bool                  // has the current view acked by its primary
	pttl     int                   // ttl of current primary
	bttl     int                   // ttl of current backup 
	idle_servers map[string]int    // extra servers, server address -> ttl
}

func create_view(viewno uint, primary string, backup string) (view *View) {
	view = new(View)
	view.Viewnum = viewno
	view.Primary = primary
	view.Backup  = backup
	return
}

func (vs *ViewServer) print_view() {
	if vs.view == nil {
		DPrintf("no view in the view server\n");
	} else {
		DPrintf("view : %d : %s : %s\n", vs.view.Viewnum, vs.view.Primary, vs.view.Backup)
	}
}

//
// server Ping RPC handler.
//
func (vs *ViewServer) Ping(args *PingArgs, reply *PingReply) error {
	// Your code here.
	vs.mu.Lock()
	defer vs.mu.Unlock()

	client, viewno := args.Me, args.Viewnum

	DPrintf("RPC Ping : viewno %d from client %s\n", viewno, client);
	vs.print_view()
	
	if viewno == 0 {
		if vs.view == nil {
			// first server, let it be the primary
			vs.view = create_view(1, client, "")
		} else {
			// restarted p is treated as dead
			if client == vs.view.Primary { 
				DPrintf("primary was restarted\n");
				vs.pttl = 0;  // set primary dead
				// if the current view is acked by its primary, try to switch to 
				// a new view
				if (vs.packed && vs.switch_to_new_view()) {
					vs.packed = false
				}
			} 
			// handle extra servers
			if client != "" && client != vs.view.Backup { 
				// if a client is not a server, it may steal the whole database ;)
				vs.idle_servers[client] = DeadPings
			}

			// if client == vs.view.Backup, we do nothing
		}
	} else {
		if client == vs.view.Primary {
			if viewno == vs.view.Viewnum {
				DPrintf("primary Acked the %d-th view\n", viewno);
				// try to switch to a new view
				if (vs.do_view_switch()) {
					vs.packed = false
				} else {
					vs.packed = true
				}
			}
		}
	}
	
	// update TTLs
	if client == vs.view.Primary {
		vs.pttl = DeadPings
	} else if client == vs.view.Backup {
		vs.bttl = DeadPings
	} else {
		vs.idle_servers[client] = DeadPings
	}
	
	reply.View = *vs.view
	return nil
}

//
// server Get() RPC handler.
//
func (vs *ViewServer) Get(args *GetArgs, reply *GetReply) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if vs.view == nil {
		reply.View = *create_view(0, "", "")
	} else {
		reply.View = *vs.view
	}	
	return nil
}

func (vs *ViewServer) update_newv(primary string, backup string) {
	if vs.view == nil { return }
	if vs.newv == nil {
		vs.newv = create_view(vs.view.Viewnum + 1, primary, backup)
	} else {
		vs.newv.Primary = primary
		vs.newv.Backup = backup
	}
}

func get_and_del(idle_servers *map[string]int) string {
	for server := range *idle_servers {
		delete(*idle_servers, server)
		DPrintf("select server : %s\n", server);
		return server
	}
	return ""
}

//
// The view service proceeds to a new view 
//     1) if it hasn't received recent Pings from both primary and backup
//     2) if the primary or backup crashed and restarted
//     3) if there is no backup and there is an idle server
//
// Before we call this func, we should ensure that the primary has acked to
// the current viewnum (vs.view.Viewnnum)
//
func (vs *ViewServer) switch_to_new_view() bool {
	view = vs.view
	if view.Backup == "" && len(vs.idle_servers) == 0 {
		return false
	}
	if vs.pttl > 0 && vs.bttl <= 0 {
		// case 2, 3 : there is no backup or the backup is dead
		vs.update_newv(view.Primary, get_and_del(&vs.idle_servers))
	} else if vs.pttl <= 0 && vs.bttl > 0 {
		// case 2 : the primary crashed or restarted
		vs.update_newv(view.Backup, get_and_del(&vs.idle_servers))
	} else if vs.pttl <= 0 && vs.bttl <= 0 {   
		// case 1 : both primary and backup are dead in our view
		// uninitialized idle servers cannot be promoted to primary
		vs.update_newv("", "")
	}
	return vs.do_view_switch()
}

func (vs *ViewServer) do_view_switch() bool {
	if vs.newv != nil {
		vs.view, vs.newv = vs.newv, nil
		return true
	}
	return false
}

func (vs *ViewServer) cleanup_extra_servers() {
	for server := range vs.idle_servers {
		if vs.idle_servers[server] <= 0 {
			delete(vs.idle_servers, server)
		} else {
			vs.idle_servers[server]--
		}
	}	
}

//
// tick() is called once per PingInterval; it should notice
// if servers have died or recovered, and change the view
// accordingly.
//
func (vs *ViewServer) tick() {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	
	if vs.view == nil { return }

	// clean extra servers which are treated as dead
	vs.cleanup_extra_servers()
	
	// if pimary has acked current view, try to switch to the new view
	if (vs.packed && vs.switch_to_new_view()) {
		vs.packed = false
	}

	// case : no primary and no backup
	if vs.view.Primary == "" { vs.pttl = 0 }
	// case : old backup is promoted to primary
	//        and there're no idle servers 
	if vs.view.Backup  == "" { vs.bttl = 0 }
	
	if vs.pttl > 0 { vs.pttl-- }
	if vs.bttl > 0 { vs.bttl-- }
}

//
// tell the server to shut itself down.
// for testing.
// please don't change these two functions.
//
func (vs *ViewServer) Kill() {
	atomic.StoreInt32(&vs.dead, 1)
	vs.l.Close()
}

//
// has this server been asked to shut down?
//
func (vs *ViewServer) isdead() bool {
	return atomic.LoadInt32(&vs.dead) != 0
}

// please don't change this function.
func (vs *ViewServer) GetRPCCount() int32 {
	return atomic.LoadInt32(&vs.rpccount)
}

func StartServer(me string) *ViewServer {
	vs := new(ViewServer)
	vs.me = me
	// Your vs.* initializations here.
	vs.idle_servers = make(map[string] int)

	// tell net/rpc about our RPC server and handlers.
	rpcs := rpc.NewServer()
	rpcs.Register(vs)

	// prepare to receive connections from clients.
	// change "unix" to "tcp" to use over a network.
	os.Remove(vs.me) // only needed for "unix"
	l, e := net.Listen("unix", vs.me)
	if e != nil {
		log.Fatal("listen error: ", e)
	}
	vs.l = l

	// please don't change any of the following code,
	// or do anything to subvert it.

	// create a thread to accept RPC connections from clients.
	go func() {
		for vs.isdead() == false {
			conn, err := vs.l.Accept()
			if err == nil && vs.isdead() == false {
				atomic.AddInt32(&vs.rpccount, 1)
				go rpcs.ServeConn(conn)
			} else if err == nil {
				conn.Close()
			}
			if err != nil && vs.isdead() == false {
				fmt.Printf("ViewServer(%v) accept: %v\n", me, err.Error())
				vs.Kill()
			}
		}
	}()

	// create a thread to call tick() periodically.
	go func() {
		for vs.isdead() == false {
			vs.tick()
			time.Sleep(PingInterval)
		}
	}()

	return vs
}
