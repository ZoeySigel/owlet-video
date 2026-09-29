#!/usr/bin/env python3
"""Root-only GCFeed migration rehearsal and guarded production cutover.
Keeps source DB/media and durable backups. Never performs reverse data migration.
"""
import argparse, datetime, hashlib, json, os, pathlib, re, shlex, shutil, subprocess, time, urllib.request

APP_ENV = pathlib.Path('/etc/owlet-video/app.env')
BASE = pathlib.Path('/srv/owlet-video')
TARGET_DB = 'owlet_gcfeed'
TARGET_DATA = pathlib.Path('/var/lib/owlet-video-core')
SERVICES = ('owlet-video-api', 'owlet-video-worker')

def run(args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, text=True, capture_output=True, **kwargs).stdout.strip()

def sql(query, database=None):
    args=['mysql','--batch','--skip-column-names']
    if database: args.append(database)
    return run(args,input=query)

def env_read():
    result={}
    for line in APP_ENV.read_text().splitlines():
        if line.strip() and not line.lstrip().startswith('#') and '=' in line:
            k,v=line.split('=',1);result[k]=''.join(shlex.split(v))
    return result

def database(dsn):
    m=re.search(r'\)/([a-zA-Z0-9_]+)(?:\?|$)',dsn)
    if not m: raise RuntimeError('unsupported database DSN')
    return m.group(1)

def replace_database(dsn,name):
    old=database(dsn)
    return dsn.replace(')/'+old,')/'+name,1)

def write_env(path,env,mode=0o640):
    path.write_text(''.join(k+'='+shlex.quote(str(v))+'\n' for k,v in env.items()))
    path.chmod(mode)

def create_database(name,user):
    if not re.fullmatch(r'[a-zA-Z0-9_]+',name+user):raise RuntimeError('unsafe SQL identifier')
    if int(sql("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='"+name+"'")):
        raise RuntimeError('target database is not empty: '+name)
    sql('CREATE DATABASE IF NOT EXISTS `'+name+'` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci; GRANT ALL ON `'+name+"`.* TO '"+user+"'@'127.0.0.1';")

def dump(name,path):
    with path.open('w') as stream:
        subprocess.run(['mysqldump','--single-transaction','--no-tablespaces','--set-gtid-purged=OFF',name],stdout=stream,stderr=subprocess.PIPE,text=True,check=True)
    path.chmod(0o600)

def migrate(release,env,source_db,target_db,source_data,target_data):
    migration=dict(os.environ,**env)
    migration.update(SOURCE_MYSQL_DSN=replace_database(env['MYSQL_DSN'],source_db),TARGET_MYSQL_DSN=replace_database(env['MYSQL_DSN'],target_db),SOURCE_DATA_DIR=str(source_data),TARGET_DATA_DIR=str(target_data),SOURCE_RUNTIME_DATA_DIR=env['DATA_DIR'],TARGET_RUNTIME_DATA_DIR=str(TARGET_DATA),MIGRATION_SOURCE_QUIESCED='1')
    output=run([release/'bin/owlet-video','migrate-core','apply'],env=migration)
    lines=output.splitlines()
    value=json.loads(lines[-1])
    if value.get('state')!='verified':raise RuntimeError('migration did not verify')
    return value

def queue_counts(vhost):
    rows=run(['rabbitmqctl','-q','list_queues','-p',vhost,'name','messages_ready','messages_unacknowledged'])
    return {p[0]:int(p[1])+int(p[2]) for line in rows.splitlines() if len(p:=line.split())==3 and p[0].startswith('owlet.')}

def healthy():
    for _ in range(30):
        try:
            with urllib.request.urlopen('http://127.0.0.1:8080/healthz',timeout=3) as response:value=json.load(response)
            with urllib.request.urlopen('http://127.0.0.1:8080/api/v1/capabilities',timeout=3) as response:cap=json.load(response)
            if value.get('status')=='ok' and value.get('writes',{}).get('status')=='ok' and cap.get('engine')=='gcfeed' and all(run(['systemctl','is-active',s])=='active' for s in SERVICES):return
        except Exception:pass
        time.sleep(2)
    raise RuntimeError('core health checks did not pass')

def main():
    p=argparse.ArgumentParser();p.add_argument('mode',choices=['rehearse','cutover']);p.add_argument('--release',required=True);args=p.parse_args()
    if os.geteuid()!=0:raise RuntimeError('run as root')
    release=pathlib.Path(args.release).resolve()
    if release.parent!=BASE/'releases' or not (release/'bin/owlet-video').is_file() or not (release/'web/index.html').is_file():raise RuntimeError('invalid release')
    env=env_read();source_db=database(env['MYSQL_DSN']);source_data=pathlib.Path(env['DATA_DIR']).resolve();user=env['MYSQL_DSN'].split(':',1)[0]
    if env.get('BACKEND_ENGINE')=='gcfeed' or source_db==TARGET_DB or source_data==TARGET_DATA:raise RuntimeError('already on core; refusing replay')
    stamp=datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    backup=pathlib.Path('/var/backups/owlet-video-cutover')/stamp
    backup.mkdir(parents=True,mode=0o700);backup.chmod(0o700)
    if args.mode=='rehearse':
        src='owlet_rehearsal_source_'+stamp.lower();dst='owlet_rehearsal_target_'+stamp.lower()
        create_database(src,user);create_database(dst,user)
        dump(source_db,backup/'source.sql')
        with (backup/'source.sql').open() as f:subprocess.run(['mysql',src],stdin=f,check=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        # Snapshot files are physically copied; no writable hard links to live data.
        shutil.copytree(source_data,backup/'source-data')
        result=migrate(release,env,src,dst,backup/'source-data',backup/'target-data')
        result.update(mode='rehearsal',backup=str(backup),targetDatabase=dst,binarySHA256=hashlib.sha256((release/'bin/owlet-video').read_bytes()).hexdigest())
        (backup/'result.json').write_text(json.dumps(result,indent=2));print(json.dumps(result),flush=True);return
    if not list(pathlib.Path('/var/backups/owlet-video-cutover').glob('*/result.json')):raise RuntimeError('rehearsal required')
    binary_hash=hashlib.sha256((release/'bin/owlet-video').read_bytes()).hexdigest()
    if not any(json.loads(p.read_text()).get('binarySHA256')==binary_hash for p in pathlib.Path('/var/backups/owlet-video-cutover').glob('*/result.json')):raise RuntimeError('this binary has not passed rehearsal')
    redisenv=dict(os.environ,REDISCLI_AUTH=env.get('REDIS_PASSWORD',''))
    if run(['redis-cli','-n','1','DBSIZE'],env=redisenv)!='0':raise RuntimeError('target Redis DB 1 is not empty')
    create_database(TARGET_DB,user)
    old_vhost=env['RABBITMQ_URL'].rsplit('/',1)[1]
    if queue_counts(old_vhost).get('owlet.dead',0):raise RuntimeError('source has dead letters')
    rabbit_user=env['RABBITMQ_URL'].split('://',1)[1].split(':',1)[0]
    run(['rabbitmqctl','add_vhost','owlet_gcfeed'])
    run(['rabbitmqctl','set_permissions','-p','owlet_gcfeed',rabbit_user,'.*','.*','.*'])
    paths=[APP_ENV,pathlib.Path('/etc/caddy/Caddyfile'),pathlib.Path('/usr/local/sbin/owlet-video-backup'),pathlib.Path('/usr/local/libexec/owlet-video-monitor.py'),pathlib.Path('/etc/systemd/system/owlet-video-monitor.service'),pathlib.Path('/etc/owlet-video/monitor.env')]
    for s in SERVICES:paths.append(pathlib.Path('/etc/systemd/system')/(s+'.service.d')/'gcfeed.conf')
    files={}
    for i,path in enumerate(paths):
        dest=backup/('config-'+str(i));files[str(path)]=str(dest) if path.exists() else None
        if path.exists():shutil.copy2(path,dest)
    previous=str((BASE/'current').resolve())
    state={'backup':str(backup),'previousRelease':previous,'newRelease':str(release),'configs':files,'sourceDatabase':source_db,'sourceData':str(source_data),'targetDatabase':TARGET_DB,'targetData':str(TARGET_DATA),'trafficOpened':False}
    (backup/'state.json').write_text(json.dumps(state,indent=2))
    def link(target):
        temporary=BASE/'.gcfeed-current';temporary.symlink_to(target);os.replace(temporary,BASE/'current')
    try:
        run(['systemctl','stop','owlet-video-monitor.timer','owlet-video-backup.timer'])
        run(['systemctl','stop','owlet-video-backup.service'])
        original_caddy=pathlib.Path(files['/etc/caddy/Caddyfile']).read_text()
        pathlib.Path('/etc/caddy/Caddyfile').write_text('owl-et.me {\n header Retry-After 120\n respond "Backend upgrade in progress. Please retry shortly." 503\n}\n')
        run(['caddy','validate','--config','/etc/caddy/Caddyfile']);run(['systemctl','reload','caddy'])
        run(['systemctl','stop','owlet-video-api'])
        for _ in range(30):
            pending=int(sql('SELECT COUNT(*) FROM outboxes WHERE published_at IS NULL',source_db))+int(sql('SELECT COUNT(*) FROM interaction_commands WHERE completed_at IS NULL',source_db))
            if pending==0 and sum(queue_counts(old_vhost).values())==0:break
            time.sleep(2)
        else:raise RuntimeError('source worker did not drain')
        run(['systemctl','stop','owlet-video-worker'])
        dump(source_db,backup/'source.sql');shutil.copytree(source_data,backup/'source-data')
        result=migrate(release,env,source_db,TARGET_DB,source_data,TARGET_DATA)
        (backup/'migration.json').write_text(json.dumps(result,indent=2))
        run(['chown','-R','owlet-video:owlet-video',TARGET_DATA])
        core=dict(env,BACKEND_ENGINE='gcfeed',MYSQL_DSN=replace_database(env['MYSQL_DSN'],TARGET_DB),REDIS_DB='1',RABBITMQ_URL=env['RABBITMQ_URL'].rsplit('/',1)[0]+'/owlet_gcfeed',DATA_DIR=str(TARGET_DATA),BACKUP_DATABASE=TARGET_DB)
        write_env(APP_ENV,core);run(['chown','root:owlet-video',APP_ENV])
        for s in SERVICES:
            drop=pathlib.Path('/etc/systemd/system')/(s+'.service.d')/'gcfeed.conf';drop.parent.mkdir(parents=True,exist_ok=True)
            drop.write_text('[Service]\nReadWritePaths=\nReadWritePaths='+str(TARGET_DATA)+'\n')
        shutil.copy2(release/'deploy/backup.sh','/usr/local/sbin/owlet-video-backup');pathlib.Path('/usr/local/sbin/owlet-video-backup').chmod(0o755)
        shutil.copy2(release/'deploy/monitor.py','/usr/local/libexec/owlet-video-monitor.py')
        shutil.copy2(release/'deploy/owlet-video-monitor.service','/etc/systemd/system/owlet-video-monitor.service')
        write_env(pathlib.Path('/etc/owlet-video/monitor.env'),{'BACKEND_ENGINE':'gcfeed','RABBITMQ_VHOST':'owlet_gcfeed'},0o600)
        link(release);run(['systemctl','daemon-reload']);run(['systemctl','start',*SERVICES]);healthy()
        with urllib.request.urlopen('http://127.0.0.1:8080/api/v1/videos?sort=latest',timeout=5) as r:feed=json.load(r)
        if result['counts']['videos'] and not feed.get('items'):raise RuntimeError('migrated feed is empty')
        for v in feed.get('items',[])[:5]:run(['runuser','-u','caddy','--','test','-r',str(TARGET_DATA/v['playUrl'].lstrip('/'))])
        pathlib.Path('/etc/caddy/Caddyfile').write_text(original_caddy.replace(str(source_data),str(TARGET_DATA)))
        run(['caddy','validate','--config','/etc/caddy/Caddyfile']);run(['systemctl','reload','caddy'])
        state['trafficOpened']=True;(backup/'state.json').write_text(json.dumps(state,indent=2))
        run(['systemctl','start','owlet-video-backup.timer','owlet-video-monitor.timer'])
        print(json.dumps(dict(result,mode='cutover',backup=str(backup),release=str(release),trafficOpened=True)),flush=True)
    except Exception:
        if not state['trafficOpened']:
            subprocess.run(['systemctl','stop',*SERVICES],capture_output=True)
            for path,saved in files.items():
                path=pathlib.Path(path)
                if saved:shutil.copy2(saved,path)
                elif path.exists():path.unlink()
            link(previous);run(['systemctl','daemon-reload']);run(['systemctl','start',*SERVICES]);run(['systemctl','reload','caddy']);run(['systemctl','start','owlet-video-backup.timer','owlet-video-monitor.timer'])
            print('Cutover failed before opening traffic; restored old configuration and release.',flush=True)
        else:print('Traffic was opened; automatic database rollback is disabled to preserve new writes.',flush=True)
        raise

if __name__=='__main__':main()
