"""mlx-audio's server pinned to one model, preloaded at start.

usage: gpuworks-mlx-audio --model PATH --host HOST --port PORT
mlx-audio loads whatever model a request names; this instance serves only
--model, whatever the request says, and reports /health once it is loaded.
"""
import argparse


def main():
    parser = argparse.ArgumentParser(prog="gpuworks-mlx-audio")
    parser.add_argument("--model", required=True)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8000)
    args = parser.parse_args()

    import uvicorn
    from mlx_audio import server

    provider = server.model_provider
    load = provider.load_model
    load(args.model)
    provider.load_model = lambda _name: load(args.model)

    @server.app.get("/health")
    async def health():
        return {"status": "ok", "model": args.model}

    uvicorn.run(server.app, host=args.host, port=args.port, workers=1, loop="asyncio")


if __name__ == "__main__":
    main()
