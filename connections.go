package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jaracil/smartio"
	"golang.org/x/net/context"
)

type JsonRpcErr struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type JsonRpcReq struct {
	Jsonrpc string      `json:"jsonrpc"`
	Id      interface{} `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	nc      *NexusConn
}

type JsonRpcRes struct {
	Jsonrpc string      `json:"jsonrpc"`
	Id      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *JsonRpcErr `json:"error,omitempty"`
}

type NexusConn struct {
	conn      net.Conn
	connRx    *smartio.SmartReader
	connTx    *smartio.SmartWriter
	connId    string
	user      *UserData
	chRes     chan *JsonRpcRes
	chReq     chan *JsonRpcReq
	context   context.Context
	cancelFun context.CancelFunc
	wdog      int64
}

func NewNexusConn(conn net.Conn) *NexusConn {
	nc := &NexusConn{
		conn:   conn,
		connRx: smartio.NewSmartReader(conn),
		connTx: smartio.NewSmartWriter(conn),
		connId: nodeId + safeId(4),
		chRes:  make(chan *JsonRpcRes, 64),
		chReq:  make(chan *JsonRpcReq, 64),
		wdog:   90,
	}
	nc.context, nc.cancelFun = context.WithCancel(mainContext)
	return nc
}

func (req *JsonRpcReq) Error(code int, message string, data interface{}) {
	if code < 0 {
		if message != "" {
			message = fmt.Sprintf("%s:[%s]", ErrStr[code], message)
		} else {
			message = ErrStr[code]
		}
	}
	req.nc.pushRes(
		&JsonRpcRes{
			Id:    req.Id,
			Error: &JsonRpcErr{Code: code, Message: message, Data: data},
		},
	)
}

func (req *JsonRpcReq) Result(result interface{}) {
	req.nc.pushRes(
		&JsonRpcRes{
			Id:     req.Id,
			Result: result,
		},
	)
}

func (nc *NexusConn) pushRes(res *JsonRpcRes) (err error) {
	select {
	case nc.chRes <- res:
	case <-nc.context.Done():
		err = errors.New("Context cancelled")
	}
	return
}

func (nc *NexusConn) pullRes() (res *JsonRpcRes, err error) {
	select {
	case res = <-nc.chRes:
	case <-nc.context.Done():
		err = errors.New("Context cancelled")
	}
	return
}

func (nc *NexusConn) pushReq(req *JsonRpcReq) (err error) {
	select {
	case nc.chReq <- req:
	case <-nc.context.Done():
		err = errors.New("Context cancelled")
	}
	return
}

func (nc *NexusConn) pullReq() (req *JsonRpcReq, err error) {
	select {
	case req = <-nc.chReq:
	case <-nc.context.Done():
		err = errors.New("Context cancelled")
	}
	return
}

func (nc *NexusConn) getTags(prefix string) (tags map[string]interface{}) {
	tags = map[string]interface{}{}
	if nc.user == nil || nc.user.Tags == nil {
		return
	}
	pfs := prefixes(prefix)
	for _, pf := range pfs {
		if tm, ok := nc.user.Tags[pf]; ok {
			for k, v := range tm {
				tags[k] = v
			}
		}
	}
	return
}

func (nc *NexusConn) handleReq(req *JsonRpcReq) {
	if req.Id == nil {
		return
	}
	switch {
	case strings.HasPrefix(req.Method, "sys."):
		nc.handleSysReq(req)
	case strings.HasPrefix(req.Method, "task."):
		nc.handleTaskReq(req)
	case strings.HasPrefix(req.Method, "pipe."):
		nc.handlePipeReq(req)
	case strings.HasPrefix(req.Method, "chan."):
		nc.handleChanReq(req)
	case strings.HasPrefix(req.Method, "user."):
		nc.handleUserReq(req)
	case strings.HasPrefix(req.Method, "sync."):
		nc.handleSyncReq(req)
	default:
		req.Error(ErrMethodNotFound, "", nil)
	}
}

func (nc *NexusConn) respWorker() {
	defer nc.cancelFun()
	trackCh, err := sesNotify.Register(nc.connId, make(chan interface{}, 1024))
	if err != nil { // Duplicated session ???
		return
	}
	defer sesNotify.Unregister(nc.connId)
	for {
		select {
		case d := <-trackCh:
			resTask := d.(*Task)
			if resTask.ErrCode != nil {
				nc.pushRes(
					&JsonRpcRes{
						Id:    resTask.LocalId,
						Error: &JsonRpcErr{Code: *resTask.ErrCode, Message: resTask.ErrStr, Data: resTask.ErrObj},
					},
				)
			} else {
				nc.pushRes(
					&JsonRpcRes{
						Id:     resTask.LocalId,
						Result: resTask.Result,
					},
				)
			}
		case <-nc.context.Done():
			return
		}
	}
}

func (nc *NexusConn) sendWorker() {
	defer nc.cancelFun()
	var null *int
	for {
		res, err := nc.pullRes()
		if err != nil {
			log.Print("error on sendWorker:", err)
			break
		}
		if res.Id == nil {
			if res.Error == nil {
				continue //Skip notification responses
			}
			if res.Error.Code == ErrInvalidRequest || res.Error.Code == ErrParse {
				res.Id = null
			} else {
				continue
			}
		}
		res.Jsonrpc = "2.0"
		if res.Result == nil && res.Error == nil {
			res.Result = null
		}
		buf, err := json.Marshal(res)
		if err != nil {
			log.Print("Marshal error: ", err)
			break
		}
		buf = append(buf, byte('\r'), byte('\n'))
		n, err := nc.connTx.Write(buf)
		if err != nil || n != len(buf) {
			log.Print("Connection write error: ", err)
			break
		}
	}
	log.Print("exit from sendWorker")
}

func (nc *NexusConn) recvWorker() {
	defer nc.cancelFun()
	dec := json.NewDecoder(nc.connRx)
	for dec.More() {
		req := &JsonRpcReq{nc: nc}
		nc.connRx.SetLimit(1024 * 1024 * 32)
		err := dec.Decode(req)
		if err != nil {
			if _, ok := err.(*json.SyntaxError); ok {
				req.Error(ErrParse, "", nil)
				dec = json.NewDecoder(nc.connRx) // Refresh decoder
				continue
			}
			if _, ok := err.(*json.UnmarshalTypeError); ok {
				req.Error(ErrInvalidRequest, "", nil)
				continue
			}
			break
		}
		err = nc.pushReq(req)
		if err != nil {
			log.Print("error on recvWorker:", err)
			break
		}
	}
	log.Print("exit from recvWorker")
}

func (nc *NexusConn) watchdog() {
	defer nc.cancelFun()
	tick := time.NewTicker(time.Second * 10)
	exit := false
	for !exit {
		select {
		case <-tick.C:
			now := time.Now().Unix()
			max := atomic.LoadInt64(&nc.wdog)
			if (now-nc.connRx.GetLast() > max) &&
				(now-nc.connTx.GetLast() > max) {
				exit = true
				log.Printf("Connection [%s] watch dog expired!", nc.connId)
			}

		case <-nc.context.Done():
			exit = true
		}
	}
	tick.Stop()
}

func (nc *NexusConn) clean() {
	nc.cancelFun()
	nc.conn.Close()
	if mainContext.Err() == nil {
		log.Printf("Clean %s session\r\n", nc.connId)
		dbClean(nc.connId)
	}
}

func (nc *NexusConn) handle() {
	defer nc.clean()
	go nc.respWorker()
	go nc.sendWorker()
	go nc.recvWorker()
	go nc.watchdog()
	for {
		req, err := nc.pullReq()
		if err != nil {
			log.Print("error on handle:", err)
			break
		}
		log.Printf("Recibida instruccion jsonrpc: %+v", req)
		if req.Jsonrpc != "2.0" || req.Method == "" {
			req.Error(ErrInvalidRequest, "", nil)
			continue
		}
		go nc.handleReq(req)
	}
}