'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
const html=fs.readFileSync(path.join(__dirname,'../template.html'),'utf8');
const body=html.slice(html.indexOf('const formatCurrencyValue ='),html.indexOf('const formatBillingAmount ='));
const context={ECSBillingDisplay:require('../static/billing-display.js')};vm.createContext(context);vm.runInContext(body+'globalThis.format=formatCurrencyValue;',context);
test('account money never treats missing or invalid data as zero',()=>{for(const value of [null,undefined,'',' ',false,true,NaN,Infinity,'unavailable'])assert.equal(context.format(value,'USD'),'--');assert.equal(context.format(0,'USD'),'$0');});
test('account summary uses same six-decimal precision as billing history',()=>{for(const value of [0.002925,0.13259,9.123456,-0.0117])assert.equal(context.format(value,'USD'),context.ECSBillingDisplay.amount(value,'USD'));});
test('history remains accessible when summary is incomplete',()=>{assert.ok(!html.includes('acc.billing.enabled && !acc.billing.error'));assert.ok(html.includes('本月消费'));assert.ok(html.includes('openBillingDetails(acc)'));});
