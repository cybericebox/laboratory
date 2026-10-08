#!/usr/bin/env python3
"""Deferred END native evidence collector. No deployment or implicit stop.

Run only after the root integrates, reviews and supplies an approved commit.
Capture before actual guarded stop, then assert release after the real RPC.
Raw identities are retained; credentials and kubeconfig contents are never read.
"""
import argparse, json, os, pathlib, re, subprocess, time

p=argparse.ArgumentParser();p.add_argument('action',choices=['inventory','released']);p.add_argument('--fixture',required=True);p.add_argument('--namespace',required=True);p.add_argument('--lab',required=True);p.add_argument('--inventory',required=True);a=p.parse_args()
commit=os.environ.get('CICE_APPROVED_SOURCE_COMMIT','')
if not re.fullmatch('[0-9a-f]{40}',commit):raise SystemExit('approved final integrated 40-character source commit required')
f=pathlib.Path(a.fixture).resolve();state=json.loads((f/'state.json').read_text())
# Purpose-built wrapper fixes scratch kubeconfig/context. Never default context.
kubectl=str(f/'kubectl.sh')
def run(*args):
 r=subprocess.run(args,check=True,capture_output=True,text=True,timeout=30);return r.stdout

def get(kind):return json.loads(run(kubectl,'-n',a.namespace,'get',kind,'-o','json'))
lab=get('lab/'+a.lab);devices=get('devices');pods=get('pods')
owned=[d for d in devices['items'] if any(o.get('kind')=='Lab' and o.get('uid')==lab['metadata']['uid'] for o in d['metadata'].get('ownerReferences',[]))]
record={'source':commit,'at':time.time(),'lab':lab,'devices':owned,'pods':pods,'scope':'Actual API + native containerd process table; OVS/freezer/ACL/restore gates separate'}
worker='cice-lifecycle-proof-20261008-worker'
# Exact container ownership is checked by prepared fixture state and parent
# deployment gates; Docker context is explicit on every request.
record['tasks']=run('docker','--context','desktop-linux','exec',worker,'ctr','-n','k8s.io','tasks','list')
out=pathlib.Path(a.inventory)
if a.action=='inventory':
 out.write_text(json.dumps(record,indent=2));out.chmod(0o600);raise SystemExit('native inventory captured; no release claimed')
before=json.loads(out.read_text())
assert before['source']==commit and before['lab']['metadata']['uid']==lab['metadata']['uid'],'source/Lab UID changed'
intent=lab['spec']['lifecycle'];status=lab['status'];ack=status['lifecycle'];alloc=status['resources']
assert ack['operationId']==intent['operationId'] and ack['revision']==intent['revision'] and ack['labUID']==lab['metadata']['uid'] and ack['observedGeneration']==lab['metadata']['generation']
assert ack['observedState']=='Stopped' and alloc['runtimeState']=='Released'
assert alloc['operationId']==intent['operationId'] and alloc['revision']==intent['revision'] and alloc['observedAt'] and alloc['releasedAt']
assert alloc['allocatedRequests']=={'cpuMillicores':0,'memoryBytes':0}
for d in owned:
 reports=d['status'].get('runtimeReports',[])
 for row in d['status'].get('runtimeInventory',[]):
  assert row['operationId']==intent['operationId'] and row['revision']==intent['revision']
  proof=next((r for r in reports if r['identity']==row),None)
  assert proof and proof['runtimeState']=='Released' and not proof.get('error')
  assert all(proof.get(k) for k in ['runtimeAbsentAt','cgroupAbsentAt','attachmentsAbsentAt','observedAt'])
  for cid in row['containerIDs']:assert cid not in record['tasks'],'API release while exact native task remains'
record['firstInventory']=str(out);record['result']='native allocation/resource release asserted; remaining gates still mandatory'
result=out.with_suffix('.released.json');result.write_text(json.dumps(record,indent=2));result.chmod(0o600)
print(str(result))
