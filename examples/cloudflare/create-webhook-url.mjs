import {randomBytes} from 'node:crypto';
import {readFileSync, writeFileSync} from 'node:fs';
const origin = new URL(process.argv[2]);
if (origin.protocol !== 'https:' || origin.username || origin.password || origin.pathname !== '/' || origin.search || origin.hash) throw new Error('Provide an HTTPS origin only');
let secret;
try {secret = readFileSync('.webhook-secret', 'utf8').trim();}
catch (error) {
  if (error.code !== 'ENOENT') throw error;
  secret = randomBytes(32).toString('hex');
  writeFileSync('.webhook-secret', secret+'\n', {mode: 0o600, flag: 'wx'});
}
if (!/^[0-9a-f]{64}$/.test(secret)) throw new Error('Invalid saved webhook secret');
console.log(`${origin.origin}/hooks/certificates/${secret}`);
