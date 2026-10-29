'use strict';
const test=require('node:test');const assert=require('node:assert/strict');const fs=require('node:fs');const os=require('node:os');const path=require('node:path');const cp=require('node:child_process');
const source=fs.readFileSync(path.join(__dirname,'../deploy.sh'),'utf8');
const start=source.indexOf('generated_token=0');const end=source.indexOf('echo "Pulling prebuilt',start);const logic=source.slice(start,end);
for(const [name,same,explicit,expected] of [['same project',true,'','existing-dummy-token'],['explicit override',true,'explicit-dummy-token','explicit-dummy-token'],['other project',false,'','generated-dummy-token']]){
 test(`deployment setup token: ${name}`,()=>{
  const dir=fs.mkdtempSync(path.join(os.tmpdir(),'ecs-token-test-'));try{
   const script=`set -euo pipefail\nECS_COMPOSE_PROJECT_NAME=project\n${explicit?`export ECS_SETUP_TOKEN=${explicit}`:'unset ECS_SETUP_TOKEN'}\ndocker(){ case "$*" in *Config.Labels*) printf '%s' '${same?'project':'other'}';; *Config.Env*) printf '%s\\n' 'OTHER=value' 'ECS_SETUP_TOKEN=existing-dummy-token';; *) exit 2;; esac; }\nopenssl(){ printf '%s' 'generated-dummy-token'; }\n${logic}\nprintf 'RESULT=%s' "$ECS_SETUP_TOKEN"\n`;
   const result=cp.spawnSync('bash',['-c',script],{cwd:dir,encoding:'utf8'});assert.equal(result.status,0,result.stderr);assert.equal(result.stdout,`RESULT=${expected}`);
  }finally{fs.rmSync(dir,{recursive:true,force:true});}
 });
}
