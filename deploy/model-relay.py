#!/usr/bin/env python3
"""Forward a Docker bridge address to a model service on the host loopback."""
import argparse
import asyncio
import logging


async def pipe(reader, writer):
    try:
        while data := await reader.read(65536):
            writer.write(data)
            await writer.drain()
        if writer.can_write_eof():
            writer.write_eof()
    except (ConnectionError, OSError):
        pass


async def main(args):
    async def forward(reader, writer):
        try:
            upstream_reader, upstream_writer = await asyncio.open_connection(args.target_host, args.target_port)
            try:
                await asyncio.gather(pipe(reader, upstream_writer), pipe(upstream_reader, writer))
            finally:
                upstream_writer.close()
                await upstream_writer.wait_closed()
        except OSError as exc:
            logging.warning("model service connection failed: %s", exc)
        finally:
            writer.close()
            try:
                await writer.wait_closed()
            except OSError:
                pass

    server = await asyncio.start_server(forward, args.listen_host, args.listen_port)
    async with server:
        await server.serve_forever()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--listen-host", required=True)
    parser.add_argument("--listen-port", type=int, required=True)
    parser.add_argument("--target-host", default="127.0.0.1")
    parser.add_argument("--target-port", type=int, required=True)
    logging.basicConfig(level=logging.INFO)
    asyncio.run(main(parser.parse_args()))
