'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm');
const html=fs.readFileSync(require('node:path').join(__dirname,'../template.html'),'utf8');
const source=html.slice(html.indexOf('const persistConfig ='),html.indexOf('const createBackup =',html.indexOf('const persistConfig =')));
function fixture(scope){
 const state={Accounts:[{groupKey:'g'}],traffic_threshold:87,admin_password:'',Notification:{password:'********',telegram:{token:'',chat_id:''}},Ddns:{cloudflare:{token:'********'}}};
 const config={value:state},baseline={value:'previous-dirty-baseline'},calls=[],notices=[];
 const context={config,configBaseline:baseline,buildSettingsSnapshot:c=>{const {Accounts,...rest}=c;return JSON.stringify(rest)},validateAccounts:()=>{calls.push('validate');return true},fetch:async(url,options)=>{calls.push(JSON.parse(options.body));return {ok:true,json:async()=>({success:true,warnings:scope==='warning'?['账号配置已保存，但实例同步失败']:[]})}},fetchConfig:async()=>{calls.push('fetchConfig');config.value={...state,traffic_threshold:95,Accounts:[{groupKey:'canonical'}]};baseline.value='server-clean'},fetchStatusData:async()=>calls.push('status'),fetchAllInstances:async()=>calls.push('instances'),isAdmin:{value:true},openAlert:(...v)=>notices.push(v),console};
 vm.createContext(context);vm.runInContext(source+';globalThis.persist=persistConfig;',context);
 return {context,config,baseline,calls,notices};
}
test('system settings payload excludes Accounts and never reloads instances',async()=>{
 const f=fixture();await f.context.persist();const body=f.calls.find(v=>typeof v==='object');assert.equal(body.save_scope,'settings');assert.ok(!('Accounts'in body));assert.ok(!f.calls.includes('status'));assert.ok(!f.calls.includes('fetchConfig'));assert.equal(f.notices[0][0],'系统设置已保存');assert.equal(f.config.value.Notification.telegram.token,'');
});
test('account save submits only Accounts and preserves unrelated dirty settings',async()=>{
 const f=fixture('warning');await f.context.persist('账号已保存','accounts');const body=f.calls.find(v=>typeof v==='object');assert.deepEqual(Object.keys(body).sort(),['Accounts','save_scope']);assert.equal(f.config.value.traffic_threshold,87);assert.equal(f.baseline.value,'previous-dirty-baseline');assert.equal(f.notices[0][1],'info');assert.match(f.notices[0][0],/同步失败/);
});
test('newly saved secret is masked without restoring an explicitly cleared value',async()=>{
 const f=fixture();f.config.value.Notification.telegram.token='new-dummy-token';await f.context.persist();assert.equal(f.config.value.Notification.telegram.token,'********');assert.ok(!f.baseline.value.includes('new-dummy-token'));
});
