"""mlx-vlm's server pinned to the model given by --model.

usage: gpuworks-mlx-vlm --model PATH --host HOST --port PORT [mlx_vlm.server flags]
Every mlx_vlm.server flag passes through; the server preloads --model, and a
request naming any other model is served by it instead of loading that name.
"""
import argparse
import sys


def main():
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--model", required=True)
    known, _ = parser.parse_known_args()

    import mlx_vlm.server as server
    from mlx_vlm.server import cli

    from gpuworks_adapters.rewrite import PinModel

    # cli.main serves "mlx_vlm.server:app", the object this resolves to.
    server.app.add_middleware(PinModel, model=known.model)
    sys.argv[0] = "mlx_vlm.server"
    cli.main()


if __name__ == "__main__":
    main()
