package redis

import (
	"errors"
	"fmt"
	"github.com/garyburd/redigo/redis"
	"github.com/nybuxtsui/log"
	"strings"
	"sync"
	"time"
)

const (
	fullCheckInterval = 20
	pingInterval      = 60
)

type redisResp struct {
	reply interface{}
	err   error
}

type redisReq struct {
	ch   chan redisResp
	cmd  string
	args []interface{}
}

type redisConn struct {
	conn     redis.Conn
	addr     string
	id       int64
	pingTime int64
	connTime int64
}

type poolAddr struct {
	addrs []string
	pwd   string
}

type RedisPool struct {
	connCh      chan *redisConn
	badCh       chan *redisConn
	reqCh       chan *redisReq
	addrCh      chan poolAddr
	newMasterCh chan string
	poolAddr
	max   int32
	total int32
}

var (
	ErrTimeout = errors.New("timeout")
	reqsPool   = sync.Pool{
		New: func() interface{} {
			return make([]*redisReq, 0, 50)
		},
	}
	redisChanPool = sync.Pool{
		New: func() interface{} {
			return make(chan redisResp, 1)
		},
	}
	seed int64 = 0
)

func NewPool(addr []string, pwd string) *RedisPool {
	return NewPoolSize(addr, pwd, 20)
}

func NewPoolSize(addr []string, pwd string, max int) *RedisPool {
	var pool = &RedisPool{
		connCh:      make(chan *redisConn, max),
		badCh:       make(chan *redisConn, max),
		reqCh:       make(chan *redisReq, 1000),
		addrCh:      make(chan poolAddr, 10),
		newMasterCh: make(chan string, 10),
		max:         int32(max),
		poolAddr:    poolAddr{addr, pwd},
	}
	pool.start()
	return pool
}

func (pool *RedisPool) Exec(cmd string, args ...interface{}) (interface{}, error) {
	//log.Debug("Exec|%s|%v", cmd, args)
	ch := redisChanPool.Get().(chan redisResp)
	pool.reqCh <- &redisReq{ch, cmd, args}
	resp := <-ch
	redisChanPool.Put(ch)
	return resp.reply, resp.err
}

func (pool *RedisPool) NewAddr(addr poolAddr) {
	log.Debug("NewAddr|%v", addr)
	pool.addrCh <- addr
}

func makeConn(addr, pwd string) (conn *redisConn, err error) {
	log.Info("makeConn|%v", addr)
	conn = nil
	const dataTimeout = 5 * time.Second
	const connTimeout = 2 * time.Second
	var c redis.Conn
	if c, err = redis.DialTimeout("tcp", addr, connTimeout, dataTimeout, dataTimeout); err != nil {
		log.Error("makeConn|DialTimeout|%v", err)
	} else {
		if pwd != "" {
			if _, err = c.Do("AUTH", pwd); err != nil {
				log.Error("makeConn|auth|%v", err)
				c.Close()
				return
			}
		}
		if _, err = c.Do("get", "__test"); err != nil {
			log.Error("makeConn|get|%v", err)
			c.Close()
		} else {
			log.Info("makeConn|ok|%v", addr)
			var now = time.Now().Unix()
			conn = &redisConn{c, addr, seed, now + pingInterval, now}
			seed++
		}
	}
	return
}

func fetchRequests(ch chan *redisReq) (reqs []*redisReq) {
	reqs = reqsPool.Get().([]*redisReq)[:0]
	// 至少要先取1个请求
	reqs = append(reqs, <-ch)
	// 然后最多取100个，或者请求队列已空
	for {
		select {
		case req := <-ch:
			reqs = append(reqs, req)
		default:
			return
		}
		if len(reqs) == cap(reqs) {
			return
		}
	}
}

func processRequest(conn *redisConn, reqs []*redisReq) (err error) {
	var slaveError error = nil
	if conn != nil {
		//log.Debug("processRequest|%v|%v", conn.id, len(reqs))
		for _, req := range reqs {
			conn.conn.Send(req.cmd, req.args...)
		}
		var err = conn.conn.Flush()
		if err != nil {
			// 发送请求失败
			for _, req := range reqs {
				req.ch <- redisResp{nil, err}
			}
		} else {
			for _, req := range reqs {
				var ok bool
				if err != nil {
					// 判断是否处于错误状态
					// 处于错误状态就不用再receive了
					req.ch <- redisResp{nil, err}
				} else {
					var v interface{}
					v, err = conn.conn.Receive()
					req.ch <- redisResp{v, err}
					if err != nil {
						log.Error("processRequest|Receive|%v", err)
						if err, ok = err.(redis.Error); ok {
							// redis.Error表示的是具体某个请求的数据错误
							// 该类型错误不影响后续请求的处理
							if strings.HasPrefix(err.Error(), "ERR slavewrite,") {
								slaveError = err
							}
							err = nil
						}
					}
				}
			}
		}
	} else {
		// 获取连接超时
		for _, req := range reqs {
			req.ch <- redisResp{nil, ErrTimeout}
		}
	}
	reqsPool.Put(reqs)
	if slaveError != nil {
		err = slaveError
	}
	return
}

func (pool *RedisPool) cleanConn() {
	for pool.total > 0 {
		var conn *redisConn = nil
		select {
		case conn = <-pool.connCh:
			conn.conn.Close()
		case conn = <-pool.connCh:
			conn.conn.Close()
		}
		pool.total--
	}
}

func (pool *RedisPool) checkEvent() {
	// checkEvent再没任何消息的情况下会等待5秒
	for {
		timer := time.NewTimer(5 * time.Second)
		select {
		case newMaster := <-pool.newMasterCh:
			timer.Stop()
			log.Info("checkEvent|newMasterCh|%v", newMaster)
			var found = false
			for i := 0; i < len(pool.addrs); i++ {
				if pool.addrs[i] == newMaster {
					log.Info("checkNewMaster|found")
					pool.addrs[0], pool.addrs[i] = pool.addrs[i], pool.addrs[0]
					pool.cleanConn()
					found = true
					break
				}
			}
			if !found {
				log.Error("checkEvent|newMasterCh|not_found|%v", newMaster)
			}
		case newAddr := <-pool.addrCh:
			timer.Stop()
			log.Info("checkEvent|addrCh|%v", newAddr)
			pool.cleanConn()
			pool.poolAddr = newAddr
		case bad := <-pool.badCh:
			timer.Stop()
			log.Info("checkEvent|badCh|%v|%v", bad.id, bad.addr)
			pool.total--
			bad.conn.Close()
		case <-timer.C:
			return
		}
	}
}

func (pool *RedisPool) checkPool() {
	var pos = 0
	for pool.total < pool.max {
		log.Debug("checkPool|%v|%v", pool.max, pool.total)
		// 需要新的连接
		var addrs = pool.addrs
		if pos >= len(addrs) {
			// 兜了一圈了，看看其他消息吧
			log.Error("checkPool|retry_after")
			return
		}
		if conn, err := makeConn(addrs[pos], pool.pwd); err != nil {
			log.Error("checkPool|makeConn|%v", err)
			pos++
		} else {
			pool.connCh <- conn
			pool.total++
		}
	}
}

func (pool *RedisPool) testConn() {
	// testConn每次只检查一个连接
	var conn *redisConn
	select {
	case conn = <-pool.connCh:
	default:
		// 没有空闲的连接
		// 表示显现比较忙
		// 暂时就不检查了
		return
	}

	var addrs = pool.addrs
	var masterAddr = addrs[0]
	var pwd = pool.pwd
	var now = time.Now().Unix()
	if conn.addr != masterAddr {
		if newconn, err := makeConn(masterAddr, pwd); err != nil {
			log.Error("bkWorker|makeConn|%v", err)
		} else {
			// 主服务器已经能够连上了
			conn.conn.Close()
			conn = newconn
		}
	}
	if now > conn.pingTime {
		// 超过pingInterval，则ping一下连接
		if _, err := conn.conn.Do("set", "__ping", "1"); err != nil {
			if strings.HasPrefix(err.Error(), "ERR slavewrite,") {
				pool.processSlaveWrite(conn, err.Error())
			} else {
				log.Info("process|ping|%v", err)
				pool.badCh <- conn
			}
		} else {
			log.Debug("bgWorker|ping")
			pool.connCh <- conn
		}
	} else {
		pool.connCh <- conn
	}
	return
}

func (pool *RedisPool) bgWorker() {
	for {
		pool.checkPool()
		pool.testConn()
		pool.checkEvent()
	}
}

func (pool *RedisPool) processSlaveWrite(conn *redisConn, err string) {
	log.Info("receive|slavewrite")
	// 主从切换了
	if strings.HasSuffix(err, ",unknown") {
		conn.pingTime = time.Now().Add(2 * time.Second).Unix()
		pool.connCh <- conn
	} else {
		params := strings.SplitN(err, ",", 2)
		if len(params) != 2 {
			log.Error("process|slavewrite|format_error|%s", err)
		} else {
			var newip = params[1]
			log.Info("receive|new_master|%v", newip)
			pool.newMasterCh <- newip
		}
		pool.badCh <- conn
	}
}

func (pool *RedisPool) process() {
	for {
		var reqs = fetchRequests(pool.reqCh)
		var timer = time.NewTimer(2 * time.Second)
		select {
		case conn := <-pool.connCh:
			timer.Stop()
			go func() {
				var err = processRequest(conn, reqs)
				if err != nil {
					log.Info("process|processRequest|%v", err)
					if strings.HasPrefix(err.Error(), "ERR slavewrite,") {
						pool.processSlaveWrite(conn, err.Error())
					} else {
						pool.badCh <- conn
					}
				} else {
					conn.pingTime = time.Now().Add(pingInterval).Unix()
					pool.connCh <- conn
				}
			}()
		case <-timer.C:
			processRequest(nil, reqs)
		}
	}
}

func (pool *RedisPool) start() {
	go pool.bgWorker()
	go pool.process()
}
