// TLS terminator for the local stack: the pool speaks plain HTTP on loopback,
// but the runner and the browser need https. Pipes bytes raw in both
// directions so WebSocket upgrades and SSE streams pass through unbuffered.
//
//   node scripts/mock/bridge.mjs --cert FILE --key FILE \
//     [--listen 127.0.0.1:8444] [--target 127.0.0.1:8989]

import fs from 'node:fs';
import net from 'node:net';
import tls from 'node:tls';
import { parseArgs } from 'node:util';

function endpoint(value, fallbackPort) {
  const at = value.lastIndexOf(':');
  if (at < 0) return { host: value, port: fallbackPort };
  return { host: value.slice(0, at), port: Number(value.slice(at + 1)) };
}

export function createBridge({ cert, key, target }) {
  const upstream = endpoint(target, 8989);
  return tls.createServer({ cert, key }, (client) => {
    const server = net.connect(upstream.port, upstream.host);
    const drop = () => { client.destroy(); server.destroy(); };
    client.on('error', drop);
    server.on('error', drop);
    client.on('close', () => server.destroy());
    server.on('close', () => client.destroy());
    client.pipe(server);
    server.pipe(client);
  });
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const { values } = parseArgs({
    options: {
      cert: { type: 'string' },
      key: { type: 'string' },
      listen: { type: 'string', default: '127.0.0.1:8444' },
      target: { type: 'string', default: '127.0.0.1:8989' },
      help: { type: 'boolean', default: false },
    },
  });
  if (values.help || !values.cert || !values.key) {
    console.log('Usage: node scripts/mock/bridge.mjs --cert FILE --key FILE [--listen HOST:PORT] [--target HOST:PORT]');
    process.exit(values.help ? 0 : 1);
  }
  const server = createBridge({
    cert: fs.readFileSync(values.cert),
    key: fs.readFileSync(values.key),
    target: values.target,
  });
  const { host, port } = endpoint(values.listen, 8444);
  server.listen(port, host, () => console.log(`TLS bridge ${host}:${port} -> ${values.target}`));
}
