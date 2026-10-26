import json, threading, time, unittest, urllib.request
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
import serve.server as server
from serve.frontend import ChatTemplate
from serve.test_monitor import ReloadableEngine

class QueueTimingWire(unittest.TestCase):
    def test_body_free_stream_and_nonstream_actual_fifo_wait(self):
        for stream in (False, True):
            with self.subTest(stream=stream):
                self.check_fifo_wait(stream, cold=False)
    def test_cold_load_fifo_wait_included_load_duration_excluded(self):
        self.check_fifo_wait(True, cold=True)
    def test_concurrent_replies_keep_their_own_queue(self):
        tok=server.ByteTokenizer()
        engine=ReloadableEngine(tok,'Hello.',max_context=4096)
        entered,release=threading.Event(),threading.Event()
        original=engine.generate
        def held_generate(*args,**kwargs):
            if not entered.is_set():
                entered.set()
                self.assertTrue(release.wait(4))
            yield from original(*args,**kwargs)
        engine.generate=held_generate
        svc=server.Service(engine,tok,ChatTemplate(Path(server.__file__).parent/'chat_template.jinja'))
        httpd=server.serve(svc,port=0)
        base=f'http://127.0.0.1:{httpd.server_address[1]}'
        def chat():
            req=urllib.request.Request(base+'/v1/chat/completions',data=json.dumps({'messages':[{'role':'user','content':'Synthetic concurrent probe'}],'max_tokens':16}).encode(),headers={'Content-Type':'application/json'})
            with urllib.request.urlopen(req,timeout=5) as r: return json.loads(r.read())
        try:
            with ThreadPoolExecutor(max_workers=2) as pool:
                first=pool.submit(chat)
                self.assertTrue(entered.wait(3))
                second=pool.submit(chat)
                deadline=time.monotonic()+3
                while svc.status['queued']!=1 and time.monotonic()<deadline: time.sleep(0.005)
                self.assertEqual(svc.status['queued'],1)
                time.sleep(0.08)
                release.set()
                a,b=first.result(timeout=5),second.result(timeout=5)
            self.assertNotEqual(a['id'],b['id'])
            self.assertGreaterEqual(b['timings']['queue_ms'],70)
            self.assertLess(a['timings']['queue_ms'],b['timings']['queue_ms'])
            self.assertEqual(len(svc.api_requests),0)
        finally:
            release.set();httpd.shutdown();httpd.server_close()
    def check_fifo_wait(self, stream, cold):
        tok=server.ByteTokenizer()
        engine=ReloadableEngine(tok, 'Hello.', max_context=4096)
        if cold:
            engine.unload()
            original=engine.restart
            def restart():
                time.sleep(0.15)
                original()
            engine.restart=restart
        svc=server.Service(engine,tok,ChatTemplate(Path(server.__file__).parent/'chat_template.jinja'))
        self.assertFalse(svc.api_monitor)
        httpd=server.serve(svc,port=0)
        base=f'http://127.0.0.1:{httpd.server_address[1]}'
        def chat():
            req=urllib.request.Request(base+'/v1/chat/completions',data=json.dumps({'messages':[{'role':'user','content':'Synthetic queue probe'}], 'stream':stream,'max_tokens':16}).encode(),headers={'Content-Type':'application/json'})
            with urllib.request.urlopen(req,timeout=5) as r: raw=r.read().decode()
            chunks=[json.loads(x[6:]) for x in raw.splitlines() if x.startswith('data: ') and x!='data: [DONE]'] if stream else [json.loads(raw)]
            return chunks[-1]
        entered=threading.Event()
        original_load=svc.load
        def observed_load():
            entered.set()
            return original_load()
        svc.load=observed_load
        try:
            svc.fifo.acquire()
            started=time.monotonic()
            with ThreadPoolExecutor(max_workers=1) as pool:
                pending=pool.submit(chat)
                if not cold:
                    deadline=time.monotonic()+3
                    while svc.status['queued']!=1 and time.monotonic()<deadline: time.sleep(0.005)
                    self.assertEqual(svc.status['queued'],1)
                else:
                    # load waits before it increments generation's queued gauge.
                    self.assertTrue(entered.wait(3))
                time.sleep(0.08)
                svc.fifo.release()
                reply=pending.result(timeout=5)
            timings=reply.get('timings',{})
            self.assertIn('queue_ms',timings)
            self.assertGreaterEqual(timings['queue_ms'],70)
            if cold: self.assertGreater((time.monotonic()-started)*1000-timings['queue_ms'],100, 'model loading must not be queue time')
            self.assertNotIn('prompt_ms',timings, 'MockEngine does not expose a prefill clock')
            self.assertEqual(len(svc.api_requests),0, 'timing must not enable body retention')
            self.assertEqual(svc.metrics()['live']['queued'],0)
        finally:
            if svc.fifo.locked(): svc.fifo.release()
            httpd.shutdown();httpd.server_close()

if __name__=='__main__': unittest.main(verbosity=2)
