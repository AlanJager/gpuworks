"""Actual pinned HTTP handler, controlled MockEngine errors/cancel and actual MCP loop."""
import http.client,json,queue,socket,threading,time,unittest,urllib.request
from unittest import mock
from pathlib import Path
import serve.server as server
from serve.frontend import ChatTemplate
from serve.test_mcp import ToolLoop,call_script

class Boundaries(unittest.TestCase):
    def setUp(self):
        tok=server.ByteTokenizer()
        self.engine=server.MockEngine(tok,'Hello.',max_context=4096)
        self.svc=server.Service(self.engine,tok,ChatTemplate(Path(server.__file__).parent/'chat_template.jinja'))
        self.finished=queue.Queue()
        factory=server.make_handler
        def observed(svc):
            cls=factory(svc);original=cls.finish
            def finish(handler):
                self.finished.put(hasattr(svc.request_trace,'queue_wait_s') or hasattr(svc.request_trace,'queue_admitted'))
                original(handler)
            cls.finish=finish
            return cls
        with mock.patch.object(server,'make_handler',side_effect=observed):
            self.httpd=server.serve(self.svc,port=0)
        self.port=self.httpd.server_address[1]
    def tearDown(self):
        self.httpd.shutdown();self.httpd.server_close()
    def payload(self,stream=False):
        return json.dumps({'messages':[{'role':'user','content':'Synthetic boundary probe'}], 'stream':stream,'max_tokens':16}).encode()
    def post(self,stream=False,body=None):
        c=http.client.HTTPConnection('127.0.0.1',self.port,timeout=5)
        c.request('POST','/v1/chat/completions',body=body if body is not None else self.payload(stream),headers={'Content-Type':'application/json','Connection':'keep-alive'})
        r=c.getresponse();status,version,close=r.status,r.version,r.will_close
        raw=r.read().decode();c.close()
        return status,version,close,raw
    def assert_clean(self):
        self.assertFalse(self.finished.get(timeout=4),'handler thread retained request timing state')
        self.assertEqual(len(self.svc.api_requests),0,'monitor remains off')
    def test_invalid_request_then_sequential_success_http10(self):
        status,_,_,raw=self.post(body=b'{"messages":"invalid"}')
        self.assertEqual(status,400)
        self.assertNotIn('queue_ms',raw)
        self.assert_clean()
        status,version,closes,raw=self.post()
        self.assertEqual((status,version,closes),(200,10,True),'pinned server closes even with keep-alive requested')
        self.assertGreaterEqual(json.loads(raw)['timings']['queue_ms'],0)
        self.assert_clean()
    def test_stream_error_preserves_unknown_and_next_request_can_run(self):
        original=self.engine.generate
        def broken(*args,**kwargs):
            yield None
            raise ValueError('synthetic engine failure')
        self.engine.generate=broken
        status,_,_,raw=self.post(True)
        self.assertEqual(status,200)
        self.assertIn('synthetic engine failure',raw)
        self.assertNotIn('queue_ms',raw,'no successful timing event after error')
        self.assert_clean()
        self.engine.generate=original
        self.engine.alive=lambda:False
        def failed_load(): raise server.GpuBusy('synthetic model load failure')
        self.engine.restart=failed_load
        status,_,_,raw=self.post()
        self.assertEqual(status,503)
        self.assertNotIn('queue_ms',raw,'load failed before initial generation admission')
        self.assert_clean()
        del self.engine.alive,self.engine.restart
        self.assertEqual(self.post()[0],200)
        self.assert_clean()
    def test_disconnect_cancels_engine_and_cleans_scalar(self):
        started,canceled=threading.Event(),threading.Event()
        original=self.engine.generate
        def slow(ids,max_new,sampling,cancel,embeddings=None):
            started.set()
            self.assertTrue(cancel.wait(4),'client disconnect must still cancel engine')
            canceled.set()
            if False: yield None
        self.engine.generate=slow
        body=self.payload(False)
        c=socket.create_connection(('127.0.0.1',self.port),timeout=5)
        c.sendall(b'POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nContent-Length: '+str(len(body)).encode()+b'\r\n\r\n'+body)
        self.assertTrue(started.wait(3));c.close()
        self.assertTrue(canceled.wait(4))
        self.assert_clean()
        self.engine.generate=original
        self.assertEqual(self.post()[0],200)
        self.assert_clean()

class MultiRound(ToolLoop):
    def test_queue_stops_at_first_admission_after_visible_content(self):
        self.start(call_script('fake__echo',text='synthetic'),'</think>\n\nDone.')
        original=self.svc.run
        rounds=[]
        first_visible=threading.Event()
        observed={}
        def queued_run(*args,**kwargs):
            if rounds:
                self.assertTrue(first_visible.wait(3))
                self.svc.fifo.acquire()
                observed['second_wait_started']=time.monotonic()
                def release():
                    observed['second_wait_ended']=time.monotonic()
                    self.svc.fifo.release()
                timer=threading.Timer(0.2,release);timer.start()
            for kind,value in original(*args,**kwargs):
                if kind=='done': rounds.append(value['timings']['queue_ms'])
                yield kind,value
        self.svc.run=queued_run
        body={'model':'m','messages':[{'role':'user','content':'synthetic'}], 'stream':True,'strata_mcp':True}
        req=urllib.request.Request(self.base+'/v1/chat/completions',data=json.dumps(body).encode(),headers={'Content-Type':'application/json'})
        chunks=[]
        with urllib.request.urlopen(req,timeout=5) as response:
            for raw in response:
                if not raw.startswith(b'data: {'):continue
                chunk=json.loads(raw[6:]);chunks.append(chunk)
                content=chunk.get('choices',[{}])[0].get('delta',{}).get('content')
                if content and not first_visible.is_set():
                    observed['first_visible']=time.monotonic()
                    first_visible.set()
        self.assertEqual(len(rounds),2)
        self.assertIn('first_visible',observed)
        self.assertLess(observed['first_visible'],observed['second_wait_started'])
        self.assertGreaterEqual(observed['second_wait_ended']-observed['second_wait_started'],0.15)
        self.assertEqual(chunks[-1]['timings']['queue_ms'],rounds[0],
                         'post-visible MCP FIFO waits cannot be exported as pre-first-token queue')
        self.assertEqual(rounds[1],rounds[0])
        self.assertEqual(len(self.svc.api_requests),0)

if __name__=='__main__':
    suite=unittest.TestSuite()
    for cls in (Boundaries,MultiRound):
        for name in cls.__dict__:
            if name.startswith('test_'):suite.addTest(cls(name))
    result=unittest.TextTestRunner(verbosity=2).run(suite)
    raise SystemExit(not result.wasSuccessful())
