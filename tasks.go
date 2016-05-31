package main

import (
	"log"
	"strings"
	"time"

	r "github.com/dancannon/gorethink"
	"github.com/jaracil/ei"
)

type Task struct {
	Id           string      `gorethink:"id"`
	Stat         string      `gorethink:"stat"`
	Path         string      `gorethink:"path"`
	Method       string      `gorethink:"method"`
	Params       interface{} `gorethink:"params"`
	LocalId      interface{} `gorethink:"localId"`
	Tses         string      `gorethink:"tses"`
	Result       interface{} `gorethink:"result,omitempty"`
	ErrCode      *int        `gorethink:"errCode,omitempty"`
	ErrStr       string      `gorethink:"errStr,omitempty"`
	ErrObj       interface{} `gorethink:"errObj,omitempty"`
	Tags         interface{} `gorethink:"tags,omitempty"`
	CreationTime interface{} `gorethink:"creationTime,omitempty"`
	DeadLine     interface{} `gorethink:"deadLine,omitempty"`
}

type TaskFeed struct {
	Old *Task `gorethink:"old_val"`
	New *Task `gorethink:"new_val"`
}

func taskPurge() {
	defer exit("purge goroutine error")
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			r.Table("tasks").
				Between(r.MinVal, r.Now(), r.BetweenOpts{Index: "deadLine"}).
				Update(r.Branch(r.Row.Field("stat").Ne("done"),
					map[string]interface{}{"stat": "done", "errCode": ErrTimeout, "errStr": ErrStr[ErrTimeout], "deadLine": r.Now().Add(600)},
					map[string]interface{}{}),
					r.UpdateOpts{ReturnChanges: false}).
				RunWrite(db, r.RunOpts{Durability: "soft"})
			r.Table("tasks").
				Between(r.MinVal, r.Now(), r.BetweenOpts{Index: "deadLine"}).
				Filter(r.Row.Field("stat").Eq("done")).
				Delete().
				RunWrite(db, r.RunOpts{Durability: "soft"})
		case <-mainContext.Done():
			return
		}
	}
}

func taskTrack() {
	defer exit("task change-feed error")
	for retry := 0; retry < 10; retry++ {
		iter, err := r.Table("tasks").
			Between(nodeId, nodeId+"\uffff").
			Changes(r.ChangesOpts{IncludeInitial: true, Squash: false}).
			Filter(r.Row.Field("new_val").Ne(nil)).
			Pluck(map[string]interface{}{"new_val": []string{"id", "stat", "localId", "path", "method", "result", "errCode", "errStr", "errObj"}}).
			Run(db)
		if err != nil {
			log.Printf("Error opening taskTrack iterator:%s\n", err.Error())
			time.Sleep(time.Second)
			continue
		}
		retry = 0 //Reset retrys
		for {
			tf := &TaskFeed{}
			if !iter.Next(tf) {
				log.Printf("Error processing feed: %s\n", iter.Err().Error())
				iter.Close()
				break
			}
			task := tf.New
			switch task.Stat {
			case "done":
				sesNotify.Notify(tf.New.Id[0:16], task)
				go deleteTask(tf.New.Id)
			case "working":
				if strings.HasPrefix(task.Path, "@pull.") {
					go taskPull(task)
				}
			case "waiting":
				if !strings.HasPrefix(task.Path, "@pull.") {
					go taskWakeup(task)
				}
			}
		}
	}
}

func taskPull(task *Task) bool {
	prefix := task.Path
	if strings.HasPrefix(prefix, "@pull.") {
		prefix = prefix[6:]
	}
	for {
		wres, err := r.Table("tasks").
			GetAllByIndex("stat_path", []interface{}{"waiting", prefix}).
			Sample(1).
			Update(r.Branch(r.Row.Field("stat").Eq("waiting"),
				map[string]interface{}{"stat": "working", "tses": task.Id[0:16]},
				map[string]interface{}{}),
				r.UpdateOpts{ReturnChanges: true}).
			RunWrite(db, r.RunOpts{Durability: "soft"})
		if err != nil {
			break
		}
		if wres.Replaced > 0 {
			newTask := ei.N(wres.Changes[0].NewValue)
			result := make(map[string]interface{})
			result["taskid"] = newTask.M("id").StringZ()
			result["path"] = newTask.M("path").StringZ()
			result["method"] = newTask.M("method").StringZ()
			result["params"] = newTask.M("params").RawZ()
			result["tags"] = newTask.M("tags").MapStrZ()
			pres, err := r.Table("tasks").
				Get(task.Id).
				Update(r.Branch(r.Row.Field("stat").Eq("working"),
					map[string]interface{}{"stat": "done", "result": result, "deadLine": r.Now().Add(600)},
					map[string]interface{}{})).
				RunWrite(db, r.RunOpts{Durability: "soft"})
			if err != nil || pres.Replaced != 1 {
				r.Table("tasks").
					Get(result["taskid"]).
					Update(map[string]interface{}{"stat": "waiting"}).
					RunWrite(db, r.RunOpts{Durability: "soft"})
				break
			}
			return true
		}
		if wres.Unchanged > 0 {
			println("Collision!!!!")
			continue
		}
		break
	}
	r.Table("tasks").
		Get(task.Id).
		Update(r.Branch(r.Row.Field("stat").Eq("working"),
			map[string]interface{}{"stat": "waiting"},
			map[string]interface{}{})).
		RunWrite(db, r.RunOpts{Durability: "soft"})
	return false
}

func taskWakeup(task *Task) bool {
	for {
		wres, err := r.Table("tasks").
			GetAllByIndex("stat_path", []interface{}{"waiting", "@pull." + task.Path}).
			Sample(1).
			Update(r.Branch(r.Row.Field("stat").Eq("waiting"),
				map[string]interface{}{"stat": "working"},
				map[string]interface{}{})).
			RunWrite(db, r.RunOpts{Durability: "soft"})
		if err != nil {
			return false
		}
		if wres.Replaced > 0 {
			return true
		}
		if wres.Unchanged > 0 {
			continue
		}
		break
	}
	return false
}

func deleteTask(id string) {
	r.Table("tasks").Get(id).Delete().RunWrite(db, r.RunOpts{Durability: "soft"})
}

func (nc *NexusConn) handleTaskReq(req *JsonRpcReq) {
	var null *int
	switch req.Method {
	case "task.push":
		method, err := ei.N(req.Params).M("method").String()
		if err != nil {
			req.Error(ErrInvalidParams, "method", nil)
			return
		}
		params, err := ei.N(req.Params).M("params").Raw()
		if err != nil {
			req.Error(ErrInvalidParams, "params", nil)
			return
		}
		tags := nc.getTags(method)
		if !(ei.N(tags).M("@"+req.Method).BoolZ() || ei.N(tags).M("@admin").BoolZ()) {
			req.Error(ErrPermissionDenied, "", nil)
			return
		}
		path, met := getPathMethod(method)
		timeout := ei.N(req.Params).M("timeout").Float64Z()
		if timeout <= 0 {
			timeout = 60 * 60 * 24 * 10 // Ten days
		}
		task := &Task{
			Id:           nc.connId + safeId(10),
			Stat:         "waiting",
			Path:         path,
			Method:       met,
			Params:       params,
			Tags:         tags,
			LocalId:      req.Id,
			CreationTime: r.Now(),
			DeadLine:     r.Now().Add(timeout),
		}
		_, err = r.Table("tasks").Insert(task).RunWrite(db, r.RunOpts{Durability: "soft"})
		if err != nil {
			req.Error(ErrInternal, "", nil)
			return
		}
	case "task.pull":
		if req.Id == nil {
			return
		}
		prefix := ei.N(req.Params).M("prefix").StringZ()
		if prefix == "" {
			req.Error(ErrInvalidParams, "prefix", nil)
			return
		}
		if !strings.HasSuffix(prefix, ".") {
			prefix += "."
		}
		tags := nc.getTags(prefix)
		if !(ei.N(tags).M("@"+req.Method).BoolZ() || ei.N(tags).M("@admin").BoolZ()) {
			req.Error(ErrPermissionDenied, "", nil)
			return
		}
		timeout := ei.N(req.Params).M("timeout").Float64Z()
		if timeout <= 0 {
			timeout = 60 * 60 * 24 * 10 // Ten days
		}
		task := &Task{
			Id:           nc.connId + safeId(10),
			Stat:         "working",
			Path:         "@pull." + prefix,
			Method:       "",
			Params:       null,
			LocalId:      req.Id,
			CreationTime: r.Now(),
			DeadLine:     r.Now().Add(timeout),
		}
		_, err := r.Table("tasks").Insert(task).RunWrite(db, r.RunOpts{Durability: "soft"})
		if err != nil {
			req.Error(-32603, "", nil)
			return
		}

	case "task.cancel":
		id := ei.N(req.Params).M("taskid").RawZ()
		res, err := r.Table("tasks").
			Between(nc.connId, nc.connId+"\uffff").
			Filter(r.Row.Field("localId").Eq(id)).
			Update(r.Branch(r.Row.Field("stat").Ne("done"),
				map[string]interface{}{"stat": "done", "errCode": ErrCancel, "errStr": ErrStr[ErrCancel], "deadLine": r.Now().Add(600)},
				map[string]interface{}{}),
				r.UpdateOpts{ReturnChanges: false}).
			RunWrite(db, r.RunOpts{Durability: "soft"})

		if err != nil {
			req.Error(ErrInternal, "", nil)
			return
		}
		if res.Replaced > 0 {
			req.Result(map[string]interface{}{"ok": true})
		} else {
			req.Error(ErrInvalidTask, "", nil)
		}

	case "task.result":
		taskid := ei.N(req.Params).M("taskid").StringZ()
		result := ei.N(req.Params).M("result").RawZ()
		res, err := r.Table("tasks").
			Get(taskid).
			Update(map[string]interface{}{"stat": "done", "result": result, "deadLine": r.Now().Add(600)}).
			RunWrite(db, r.RunOpts{Durability: "soft"})
		if err != nil {
			req.Error(ErrInternal, "", nil)
			return
		}
		if res.Replaced > 0 {
			req.Result(map[string]interface{}{"ok": true})
		} else {
			req.Error(ErrInvalidTask, "", nil)
		}

	case "task.error":
		taskid := ei.N(req.Params).M("taskid").StringZ()
		code := ei.N(req.Params).M("code").IntZ()
		message := ei.N(req.Params).M("message").StringZ()
		data := ei.N(req.Params).M("data").RawZ()
		res, err := r.Table("tasks").
			Get(taskid).
			Update(map[string]interface{}{"stat": "done", "errCode": code, "errStr": message, "errObj": data, "deadLine": r.Now().Add(600)}).
			RunWrite(db, r.RunOpts{Durability: "soft"})
		if err != nil {
			req.Error(ErrInternal, "", nil)
			return
		}
		if res.Replaced > 0 {
			req.Result(map[string]interface{}{"ok": true})
		} else {
			req.Error(ErrInvalidTask, "", nil)
		}
	default:
		req.Error(ErrMethodNotFound, "", nil)
	}
}