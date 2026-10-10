'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const display = require('../static/billing-display.js');

const cases = [
    ['云服务器配置', 'instance-configuration', '云服务器配置'],
    ['Cloud server configuration', 'instance-configuration', '云服务器配置'],
    ['Instance Configuration', 'instance-configuration', '云服务器配置'],
    ['系统盘大小', 'system-disk', '系统盘'],
    ['System Disk Size', 'system-disk', '系统盘'],
    ['System_Disk_Size', 'system-disk', '系统盘'],
    ['公网IP保有费', 'eip-holding', '公网 IP 保有费'],
    ['Public IP Retention Fee', 'eip-holding', '公网 IP 保有费'],
    ['弹性公网IP出方向流量费', 'traffic', '公网出方向流量'],
    ['Pay-By-Data-Transfer EIP Bandwidth Plan - Outbound Traffic', 'traffic', '公网出方向流量'],
    ['ImageOS', 'imageos', '镜像操作系统']
];
for (const [name, kind, label] of cases) {
    test(`recognizes provider billing item: ${name}`, () => {
        const item = { billing_item: name, product_code: 'ecs' };
        assert.equal(display.kind(item), kind);
        assert.equal(display.itemLabels(item).item, label);
    });
}

test('specific item codes take precedence over translated names', () => {
    const item = { product_code: 'ecs', billing_item_code: 'system_disk_size', billing_item: 'Cloud server configuration' };
    assert.equal(display.kind(item), 'system-disk');
});

test('generic instance codes must be scoped to ECS', () => {
    assert.equal(display.kind({ product_code: 'ecs', billing_item_code: 'instance', billing_item: '计算资源' }), 'instance-configuration');
    const other = { product_code: 'rds', product_name: 'Database Service', billing_item_code: 'instance', billing_item: 'Database instance' };
    assert.equal(display.kind(other), 'generic');
    assert.equal(display.itemLabels(other).product, 'Database Service');
    assert.equal(display.itemLabels(other).item, 'Database instance');
});

test('snapshot capacity requires both snapshot context and a storage item', () => {
    const snapshot = { product_code: 'ecs', product_detail: 'snapshot', billing_item: 'Used Capacity' };
    assert.equal(display.kind(snapshot), 'snapshot');
    assert.equal(display.itemLabels(snapshot).item, '快照存储用量');
    assert.equal(display.itemLabels(snapshot).detail, '快照');
    assert.equal(display.kind({ ...snapshot, product_code: 'oss', product_detail: 'object storage' }), 'generic');
    assert.equal(display.kind({ ...snapshot, billing_item: 'Snapshot Requests' }), 'generic');
    assert.equal(display.kind({ product_code: 'ecs', billing_item: 'System Disk IOPS', billing_item_code: 'system_disk_iops' }), 'generic');
});

test('Chinese and English product names use the same display labels', () => {
    for (const [code, chinese, english] of [['ecs', '云服务器 ECS', 'Elastic Compute Service'], ['eip', '弹性公网IP', 'Elastic IP Address'], ['cdt', '云数据传输', 'Cloud Data Transfer']]) {
        assert.equal(display.itemLabels({ product_code: code, product_name: chinese }).product, display.itemLabels({ product_code: code, product_name: english }).product);
    }
    assert.equal(display.itemLabels({ product_name: 'Cloud Data Transfer' }).product, '云数据传输');
    assert.equal(display.itemLabels({ product_detail: 'Elastic Computing (Pay-As-You-Go)' }).detail, 'ECS 按量付费');
});

for (const [value, unit, hours] of [[7, '小时', 7], [7, 'Hour', 7], [7, 'hours', 7], [25200, 'seconds', 7], [25200, '秒', 7], [420, 'minutes', 7], [2, 'Day', 48], [0, 'Hour', 0]]) {
    test(`converts explicit time unit ${unit}`, () => assert.equal(display.timeHours(value, unit), hours));
}
for (const unit of ['', 'Piece', 'GB', 'GB/hour', 'Month', 'constructor', '__proto__', 'unknown']) {
    test(`does not infer hours from ${unit || 'a missing unit'}`, () => assert.equal(display.timeHours(7, unit), null));
}

test('compute and ImageOS rows share unit-aware billing duration display', () => {
    const a = display.usagePresentation({ billing_item: '云服务器配置', usage: 7, unit: '小时' });
    const b = display.usagePresentation({ billing_item: 'Cloud server configuration', usage: 7, unit: 'Hour' });
    assert.equal(a.primary, '账单计费时长：7 小时');
    assert.equal(a.primary, b.primary);
    assert.equal(display.usagePresentation({ billing_item: 'ImageOS', usage: 7, unit: 'Hour' }).primary, a.primary);
    const missing = display.usagePresentation({ billing_item: 'Cloud server configuration', usage: 7 });
    assert.match(missing.primary, /7.*单位未返回/);
    assert.doesNotMatch(missing.primary, /小时/);
});

test('service periods respect explicit units instead of blindly dividing by 3600', () => {
    for (const [period, unit] of [[21600, '秒'], [6, 'Hour'], [360, 'Minute']]) {
        const row = display.usagePresentation({ billing_item: 'Public IP Retention Fee', usage: 6, unit: 'Piece', service_period_seconds: period, service_period_unit: unit });
        assert.equal(row.billing, '账单保有时长：6 小时');
    }
    const unknown = display.usagePresentation({ billing_item: 'Public IP Retention Fee', usage: 6, unit: 'Piece', service_period_seconds: 21600 });
    assert.doesNotMatch(unknown.billing, /小时/);
    const zero = display.usagePresentation({ billing_item: 'Public IP Retention Fee', usage: 6, unit: 'Hour', service_period_seconds: 0, service_period_unit: '秒' });
    assert.equal(zero.billing, '账单保有时长：6 小时');
});

test('EIP count comes from current resources, never cumulative Piece usage', () => {
    const item = { billing_item: 'Public IP Retention Fee', usage: 6, unit: 'Piece', current_resource: { eip: { count: 1, bandwidth_mbps: 200, status: 'InUse' } } };
    const row = display.usagePresentation(item);
    assert.equal(row.primary, '当前 EIP：1 个');
    assert.match(row.current, /200 Mbps/);
    assert.match(row.billing, /6 Piece.*不代表当前 IP 数量/);
    assert.doesNotMatch(row.billing, /6 小时/);
    const missing = display.usagePresentation({ ...item, current_resource: undefined });
    assert.match(missing.primary, /当前数量未返回/);
    assert.doesNotMatch(missing.primary, /6 个/);
    assert.match(display.usagePresentation({ ...item, current_resource: { eip: { count: 0 } } }).primary, /当前数量未返回/);
});

test('disk capacity stays separate from historical billed usage', () => {
    const item = { billing_item: 'System Disk Size', usage: 14, unit: 'GiB', current_resource: { system_disk: { size_gib: 2, category: 'cloud_auto', status: 'InUse' } } };
    const row = display.usagePresentation(item);
    assert.equal(row.primary, '当前系统盘：2 GiB');
    assert.match(row.billing, /14 GiB.*不代表系统盘容量/);
    assert.doesNotMatch(row.billing, /小时/);
    assert.equal(display.usagePresentation({ ...item, unit: 'GiB-Hour' }).billing, '账单累计容量时长：14 GiB·小时');
    assert.doesNotMatch(display.usagePresentation({ ...item, unit: 'GiB/hour' }).billing, /累计容量时长/);
});

test('English and Chinese billing configuration keys are supported', () => {
    const chinese = display.usagePresentation({ billing_item: '系统盘大小', instance_config: '系统盘大小：2 GiB；系统盘种类：AutoPL云盘' });
    const english = display.usagePresentation({ billing_item: 'System Disk Size', instance_config: 'System Disk Size:2 GiB;System Disk Category:AutoPL云盘' });
    assert.equal(chinese.config, english.config);
    const compute = display.usagePresentation({ billing_item: 'Cloud server configuration', instance_config: 'CPU Cores=2;Memory=512 MiB;Operating System=linux' });
    assert.equal(compute.config, '账单配置：2 核 · 512 MiB · linux');
    const json = display.usagePresentation({ billing_item: 'Cloud server configuration', instance_config: JSON.stringify({ 'Instance Type': 'ecs.example', 'OS Type': 'linux' }) });
    assert.equal(json.config, '账单配置：ecs.example · linux');
});

test('zero, missing values and unknown items remain distinguishable', () => {
    assert.equal(display.number(0), '0');
    for (const value of [undefined, null, '', ' ', NaN, Infinity, true]) assert.equal(display.number(value), '');
    assert.match(display.usagePresentation({ billing_item: 'Custom fee', usage: 0, unit: 'GB' }).primary, /0 GB/);
    assert.equal(display.usagePresentation({ billing_item: 'Custom fee' }).primary, '账单用量未返回');
    const unknown = { product_name: 'Unknown Product', billing_item: 'Custom fee', product_detail: 'Original detail', usage: 12, unit: 'VendorUnit' };
    assert.deepEqual(display.itemLabels(unknown), { product: 'Unknown Product', item: 'Custom fee', detail: 'Original detail' });
    assert.match(display.usagePresentation(unknown).primary, /12 VendorUnit/);
});

test('snapshot lines stay bill quantities, not current snapshot counts', () => {
    const row = display.usagePresentation({ product_code: 'ecs', product_detail: 'snapshot', billing_item: 'Used Capacity', usage: 6.425776, unit: 'GB' });
    assert.equal(row.primary, '快照账单用量：6.425776 GB');
    assert.match(row.billing, /不代表当前实例的快照数量或容量/);
});

test('display formatting never changes original labels, quantities or amounts', () => {
    const items = [
        Object.freeze({ product_code: 'ecs', product_name: 'Elastic Compute Service', billing_item: 'Cloud server configuration', amount: 0.00462, usage: 7, unit: 'Hour' }),
        Object.freeze({ product_code: 'ecs', billing_item: 'System Disk Size', amount: 0.000975, usage: 7, unit: 'GiB' }),
        ...['cn-shenzhen', 'cn-guangzhou', 'cn-qingdao', 'cn-heyuan'].map(instance_id => Object.freeze({ product_code: 'ecs', product_detail: 'snapshot', billing_item: 'Used Capacity', amount: 0, usage: 6.425776, unit: 'GB', instance_id }))
    ];
    const before = JSON.stringify(items);
    items.forEach(item => { display.itemLabels(item); display.usagePresentation(item); });
    assert.equal(JSON.stringify(items), before);
    assert.equal(items.length, 6);
    assert.equal(items.reduce((sum, item) => sum + item.amount, 0).toFixed(6), '0.005595');
});

test('browser script exports the same helpers without CommonJS', () => {
    const context = vm.createContext({});
    const source = fs.readFileSync(path.join(__dirname, '../static/billing-display.js'), 'utf8');
    vm.runInContext(source, context);
    assert.equal(context.ECSBillingDisplay.kind({ billing_item: 'Public IP Retention Fee' }), 'eip-holding');
    assert.equal(context.ECSBillingDisplay.itemLabels({ product_name: 'Cloud Data Transfer' }).product, '云数据传输');
});
