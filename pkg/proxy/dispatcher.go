package proxy

import (
	"sync"
	"time"

	"github.com/fagongzi/gateway/pkg/expr"
	"github.com/fagongzi/gateway/pkg/pb/metapb"
	"github.com/fagongzi/gateway/pkg/plugin"
	"github.com/fagongzi/gateway/pkg/route"
	"github.com/fagongzi/gateway/pkg/store"
	"github.com/fagongzi/gateway/pkg/util"
	"github.com/fagongzi/goetty"
	"github.com/fagongzi/log"
	"github.com/fagongzi/util/hack"
	"github.com/fagongzi/util/task"
	"github.com/valyala/fasthttp"
)

type copyReq struct {
	origin     *fasthttp.Request
	api        *apiRuntime
	node       *apiNode
	to         *serverRuntime
	params     map[string][]byte
	idx        int
	requestTag string
}

func (req *copyReq) prepare() {
	if req.needRewrite() {
		// if not use rewrite, it only change uri path and query string
		realPath := req.rewiteURL()
		if "" != realPath {
			req.origin.SetRequestURI(realPath)
			req.origin.SetHost(req.to.meta.Addr)

			log.Infof("%s: dipatch node %d rewrite url to %s for copy",
				req.requestTag,
				req.idx,
				realPath)
		}
	}
}

func (req *copyReq) needRewrite() bool {
	return req.node.meta.URLRewrite != ""
}

func (req *copyReq) rewiteURL() string {
	ctx := &expr.Ctx{}
	ctx.Origin = req.origin
	ctx.Params = req.params
	return hack.SliceToString(expr.Exec(ctx, req.node.parsedExprs...))
}

type dispathNode struct {
	rd       *render
	ctx      *fasthttp.RequestCtx
	multiCtx *multiContext
	exprCtx  *expr.Ctx
	wg       *sync.WaitGroup

	requestTag           string
	idx                  int
	api                  *apiRuntime
	node                 *apiNode
	dest                 *serverRuntime
	copyTo               *serverRuntime
	res                  *fasthttp.Response
	cachedBody, cachedCT []byte
	err                  error
	code                 int
}

func (dn *dispathNode) reset() {
	*dn = emptyDispathNode
}

func (dn *dispathNode) hasRetryStrategy() bool {
	return dn.retryStrategy() != nil
}

func (dn *dispathNode) matchRetryStrategy(target int32) bool {
	for _, code := range dn.retryStrategy().Codes {
		if code == target {
			return true
		}
	}

	return false
}

func (dn *dispathNode) matchAllRetryStrategy() bool {
	return len(dn.retryStrategy().Codes) == 0
}

func (dn *dispathNode) httpOption() *util.HTTPOption {
	return &dn.node.httpOption
}

func (dn *dispathNode) retryStrategy() *metapb.RetryStrategy {
	return dn.node.meta.RetryStrategy
}

func (dn *dispathNode) hasError() bool {
	return dn.err != nil ||
		dn.code >= fasthttp.StatusBadRequest
}

func (dn *dispathNode) hasDefaultValue() bool {
	return dn.node.meta.DefaultValue != nil
}

func (dn *dispathNode) release() {
	if nil != dn.res {
		fasthttp.ReleaseResponse(dn.res)
	}
}

func (dn *dispathNode) needRewrite() bool {
	return dn.node.meta.URLRewrite != ""
}

func (dn *dispathNode) getResponseContentType() []byte {
	if len(dn.cachedCT) > 0 {
		return dn.cachedCT
	}

	if nil != dn.res {
		return dn.res.Header.ContentType()
	}

	return nil
}

func (dn *dispathNode) getResponseBody() []byte {
	if len(dn.cachedBody) > 0 {
		return dn.cachedBody
	}

	if dn.node.meta.UseDefault ||
		(dn.hasError() && dn.hasDefaultValue()) {
		return dn.node.meta.DefaultValue.Body
	}

	if nil != dn.res {
		return dn.res.Body()
	}

	return nil
}

func (dn *dispathNode) copyHeaderTo(ctx *fasthttp.RequestCtx) {
	if dn.node.meta.UseDefault ||
		(dn.hasError() && dn.hasDefaultValue()) {
		for _, hd := range dn.node.meta.DefaultValue.Headers {
			(&ctx.Response.Header).Add(hd.Name, hd.Value)
		}

		for _, ck := range dn.node.defaultCookies {
			(&ctx.Response.Header).SetCookie(ck)
		}
		return
	}

	if dn.res != nil {
		for _, h := range MultiResultsRemoveHeaders {
			dn.res.Header.Del(h)
		}
		dn.res.Header.CopyTo(&ctx.Response.Header)
	}
}

func (dn *dispathNode) maybeDone() {
	if nil != dn.wg {
		dn.multiCtx.completePart(dn.node.meta.AttrName, dn.getResponseBody())
		dn.wg.Done()
	}
}

type dispatcher struct {
	sync.RWMutex

	cnf            *Cfg
	routings       map[uint64]*routingRuntime
	route          *route.Route
	apis           map[uint64]*apiRuntime
	clusters       map[uint64]*clusterRuntime
	servers        map[uint64]*serverRuntime
	binds          map[uint64]map[uint64]*clusterRuntime
	proxies        map[string]*metapb.Proxy
	plugins        map[uint64]*metapb.Plugin
	appliedPlugins *metapb.AppliedPlugins
	checkerC       chan uint64
	watchStopC     chan bool
	watchEventC    chan *store.Evt
	analysiser     *util.Analysis
	store          store.Store
	httpClient     *util.FastHTTPClient
	tw             *goetty.TimeoutWheel
	runner         *task.Runner
	jsEngine       *plugin.Engine
}

func newDispatcher(cnf *Cfg, db store.Store, runner *task.Runner, jsEngine *plugin.Engine) *dispatcher {
	tw := goetty.NewTimeoutWheel(goetty.WithTickInterval(time.Second))
	rt := &dispatcher{
		cnf:         cnf,
		tw:          tw,
		store:       db,
		runner:      runner,
		analysiser:  util.NewAnalysis(tw),
		httpClient:  util.NewFastHTTPClient(),
		clusters:    make(map[uint64]*clusterRuntime),
		servers:     make(map[uint64]*serverRuntime),
		route:       route.NewRoute(),
		apis:        make(map[uint64]*apiRuntime),
		routings:    make(map[uint64]*routingRuntime),
		binds:       make(map[uint64]map[uint64]*clusterRuntime),
		proxies:     make(map[string]*metapb.Proxy),
		plugins:     make(map[uint64]*metapb.Plugin),
		checkerC:    make(chan uint64, 1024),
		watchStopC:  make(chan bool),
		watchEventC: make(chan *store.Evt),
		jsEngine:    jsEngine,
	}

	rt.readyToHeathChecker()
	return rt
}

func (r *dispatcher) dispatchCompleted() {
	r.RUnlock()
}

func (r *dispatcher) dispatch(req *fasthttp.Request, requestTag string) (*apiRuntime, []*dispathNode, *expr.Ctx) {
	r.RLock()

	var targetAPI *apiRuntime
	var dispathes []*dispathNode

	exprCtx := acquireExprCtx()
	exprCtx.Origin = req

	id, ok := r.route.Find(req.URI().Path(), exprCtx.AddParam)
	if ok {
		api := r.apis[id]
		if api.matches(req) {
			targetAPI = api
		}
	}

	if targetAPI == nil {
		return targetAPI, dispathes, exprCtx
	}

	if targetAPI.meta.UseDefault {
		log.Debugf("%s: match api %s, and use default force",
			requestTag,
			targetAPI.meta.Name)
	} else {
		for idx, node := range targetAPI.nodes {
			dn := acquireDispathNode()
			dn.idx = idx
			dn.api = targetAPI
			dn.node = node
			dn.exprCtx = exprCtx
			r.selectServer(req, dn, requestTag)
			dispathes = append(dispathes, dn)
		}
	}

	return targetAPI, dispathes, exprCtx
}

func (r *dispatcher) selectServer(req *fasthttp.Request, dn *dispathNode, requestTag string) {
	dn.dest = r.selectServerFromCluster(req, dn.node.meta.ClusterID)
	r.adjustByRouting(dn.api.meta.ID, req, dn, requestTag)
}

func (r *dispatcher) adjustByRouting(apiID uint64, req *fasthttp.Request, dn *dispathNode, requestTag string) {
	for _, routing := range r.routings {
		if routing.isUp() && routing.matches(apiID, req, requestTag) {
			log.Infof("%s: match routing %s, %s traffic to cluster %d",
				requestTag,
				routing.meta.Name,
				routing.meta.Status.String(),
				routing.meta.ClusterID)

			svr := r.selectServerFromCluster(req, routing.meta.ClusterID)

			switch routing.meta.Strategy {
			case metapb.Split:
				dn.dest = svr
			case metapb.Copy:
				dn.copyTo = svr
			}
			break
		}
	}
}

func (r *dispatcher) selectServerFromCluster(req *fasthttp.Request, id uint64) *serverRuntime {
	cluster, ok := r.clusters[id]
	if !ok {
		return nil
	}

	sid := cluster.selectServer(req)
	return r.servers[sid]
}
