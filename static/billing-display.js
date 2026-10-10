(function (root, factory) {
    const api = factory();
    if (typeof module === 'object' && module.exports) module.exports = api;
    else root.ECSBillingDisplay = api;
})(typeof globalThis !== 'undefined' ? globalThis : this, function () {
    'use strict';

    const token = (value) => String(value || '').normalize('NFKC').toLowerCase().replace(/[\s_\-().:：]/g, '');
    const numeric = (value) => {
        if (value == null || typeof value === 'boolean' || (typeof value === 'string' && !value.trim())) return null;
        const number = Number(value);
        return Number.isFinite(number) ? number : null;
    };
    const number = (value) => {
        const parsed = numeric(value);
        return parsed == null ? '' : parsed.toLocaleString('zh-CN', { maximumFractionDigits: 6 });
    };

    const amount = (value, currency = 'CNY', fixedDecimals = null) => {
        const parsed = Number(value ?? 0);
        if (!Number.isFinite(parsed)) return '--';
        const symbol = currency === 'USD' ? '$' : currency === 'CNY' ? '¥' : `${currency} `;
        const fixed = Number.isInteger(fixedDecimals);
        const precision = fixed ? Math.min(20, Math.max(0, fixedDecimals)) : 6;
        let formatted = parsed.toFixed(precision);
        if (!fixed) formatted = formatted.replace(/(\.\d*?[1-9])0+$|\.0+$/, '$1');
        if (Number(formatted) === 0) formatted = fixed ? (0).toFixed(precision) : '0';
        return `${symbol}${formatted}`;
    };

    const codeKinds = new Map([
        ['imageos', 'imageos'],
        ['systemdisk', 'system-disk'], ['systemdisksize', 'system-disk'],
        ['instance', 'instance-configuration'], ['instanceconfiguration', 'instance-configuration'], ['cloudserverconfiguration', 'instance-configuration'],
        ['publicipretentionfee', 'eip-holding'], ['eipretentionfee', 'eip-holding'], ['eipholdingfee', 'eip-holding'],
        ['outboundtraffic', 'traffic'], ['eipoutboundtraffic', 'traffic'],
        ['snapshotstorage', 'snapshot'], ['snapshotcapacity', 'snapshot']
    ]);
    const kind = (item) => {
        const code = token(item?.billing_item_code);
        const name = token(item?.billing_item);
        const isECS = token(item?.product_code) === 'ecs' || ['elasticcomputeservice', '云服务器ecs'].includes(token(item?.product_name));
        const byCode = codeKinds.get(code);
        if (byCode && (code !== 'instance' || isECS)) return byCode;
        const text = [item?.billing_item, item?.billing_item_code, item?.product_detail, item?.product_name, item?.product_code].map(token).join(' ');
        if ((text.includes('snapshot') || text.includes('快照')) && ['usedcapacity', 'storagecapacity', '快照容量', '快照存储容量', '存储容量', '使用容量'].some((value) => name === value || code === value)) return 'snapshot';
        if (text.includes('imageos') || text.includes('镜像操作系统')) return 'imageos';
        if (text.includes('systemdisksize') || text.includes('系统盘大小') || name === 'systemdisk' || name === '系统盘') return 'system-disk';
        if (text.includes('publicipretention') || text.includes('eipretention') || text.includes('eipholding') || text.includes('公网ip保有') || text.includes('eip保有') || (text.includes('弹性公网ip') && text.includes('保有'))) return 'eip-holding';
        if (text.includes('outboundtraffic') || text.includes('出方向流量')) return 'traffic';
        if (text.includes('cloudserverconfiguration') || text.includes('instanceconfiguration') || text.includes('云服务器配置') || (token(item?.product_code) === 'ecs' && token(item?.billing_item) === '计算资源')) return 'instance-configuration';
        return 'generic';
    };

    const itemNames = {
        imageos: '镜像操作系统',
        'system-disk': '系统盘',
        'instance-configuration': '云服务器配置',
        'eip-holding': '公网 IP 保有费',
        traffic: '公网出方向流量',
        snapshot: '快照存储用量'
    };
    const detailNames = new Map([
        ['elasticcomputingpayasyougo', 'ECS 按量付费'], ['elasticcomputeservicepayasyougo', 'ECS 按量付费'], ['云服务器ecs按量付费', 'ECS 按量付费'],
        ['clouddatatransferinternet', '公网流量'], ['云数据传输公网', '公网流量'],
        ['elasticip', '弹性公网 IP'], ['eip', '弹性公网 IP'], ['snapshot', '快照']
    ]);
    const itemLabels = (item) => {
        const code = token(item?.product_code);
        const name = token(item?.product_name);
        let product = item?.product_name || item?.product_code || '其他费用';
        if (code === 'ecs' || name === 'elasticcomputeservice' || name === '云服务器ecs') product = '云服务器 ECS';
        else if (code === 'eip' || name === 'elasticipaddress' || name === '弹性公网ip') product = '弹性公网 IP';
        else if (code === 'cdt' || name === 'clouddatatransfer' || name === '云数据传输') product = '云数据传输';
        return {
            product,
            item: itemNames[kind(item)] || item?.billing_item || item?.billing_item_code || '',
            detail: detailNames.get(token(item?.product_detail)) || item?.product_detail || ''
        };
    };

    const timeHours = (value, unit) => {
        const parsed = numeric(value);
        if (parsed == null || parsed < 0) return null;
        const factors = { s: 1 / 3600, sec: 1 / 3600, second: 1 / 3600, seconds: 1 / 3600, 秒: 1 / 3600, 分: 1 / 60, 分钟: 1 / 60, min: 1 / 60, minute: 1 / 60, minutes: 1 / 60, h: 1, hr: 1, hour: 1, hours: 1, 小时: 1, 时: 1, d: 24, day: 24, days: 24, 天: 24, 日: 24 };
        const factor = factors[token(unit)];
        return typeof factor === 'number' ? parsed * factor : null;
    };
    const serviceHours = (item) => {
        // Legacy API responses use a _seconds field for the raw service period.
        // Only the accompanying unit can establish how to interpret that value.
        const period = item?.service_period ?? item?.service_period_seconds;
        return numeric(period) > 0 ? timeHours(period, item?.service_period_unit) : null;
    };
    const capacityTimeUnit = (value) => {
        const normalized = String(value || '').normalize('NFKC').toLowerCase().replace(/[\s_\-·*×]/g, '');
        const match = normalized.match(/^(gib|gb|mib|mb|tib|tb)(hour|hours|hr|h|小时|时|day|days|d|天|日|month|months|月)$/);
        if (!match) return '';
        const sizes = { gib: 'GiB', gb: 'GB', mib: 'MiB', mb: 'MB', tib: 'TiB', tb: 'TB' };
        const duration = /^(hour|hours|hr|h|小时|时)$/.test(match[2]) ? '小时' : /^(day|days|d|天|日)$/.test(match[2]) ? '天' : '月';
        return `${sizes[match[1]]}·${duration}`;
    };
    const metric = (item) => {
        const value = number(item?.usage);
        if (!value) return '';
        const rawUnit = String(item?.unit || '').trim();
        return `${value} ${capacityTimeUnit(rawUnit) || rawUnit || '（单位未返回）'}`;
    };

    const configPairs = (value) => {
        const text = String(value || '').trim();
        if (text.startsWith('{')) {
            try {
                const object = JSON.parse(text);
                if (object && !Array.isArray(object)) return Object.entries(object).filter(([, value]) => ['string', 'number'].includes(typeof value)).map(([key, value]) => ({ key, value: String(value) }));
            } catch (_) { /* Fall back to the provider's delimited text. */ }
        }
        return text.split(/[;；]/).map((part) => {
            const index = part.search(/[:：=]/);
            return index > 0 ? { key: part.slice(0, index).trim(), value: part.slice(index + 1).trim() } : null;
        }).filter((pair) => pair?.key && pair?.value && pair.value !== '-');
    };
    const configValue = (pairs, ...keys) => pairs.find((pair) => keys.map(token).includes(token(pair.key)))?.value || '';
    const configSummary = (item, itemKind) => {
        const pairs = configPairs(item?.instance_config);
        let parts = [];
        if (itemKind === 'system-disk') {
            parts = [configValue(pairs, '系统盘大小', '系统盘', 'System Disk Size', 'SystemDiskSize'), configValue(pairs, '系统盘种类', '系统盘类型', 'System Disk Category', 'System Disk Type')];
        } else if (itemKind === 'instance-configuration' || itemKind === 'imageos') {
            const spec = configValue(pairs, '实例规格', 'Instance Type', 'Instance Specification', 'Instance Spec');
            const cpu = configValue(pairs, 'cpu核数', 'CPU Cores', 'CPU', 'vCPU', 'Number of vCPUs');
            const memory = configValue(pairs, '内存', 'Memory', 'Memory Size');
            const os = configValue(pairs, '操作系统的类型', '操作系统详情', 'Operating System', 'OS Type', 'OS');
            const compute = spec || [cpu && (/^\d+(\.\d+)?$/.test(cpu) ? `${cpu} 核` : cpu), memory].filter(Boolean).join(' · ');
            parts = [compute, os];
        } else if (itemKind === 'eip-holding') {
            const peak = configValue(pairs, '带宽峰值', 'Peak Bandwidth', 'Bandwidth', 'Bandwidth Peak');
            const line = configValue(pairs, '线路类型', 'Line Type', 'ISP');
            const numericPeak = Number(peak.replace(/[^\d.]/g, ''));
            const bandwidth = /kbps/i.test(peak) && numericPeak >= 1024 ? `${number(numericPeak / 1024)} Mbps` : peak;
            parts = [bandwidth, line];
        }
        parts = parts.filter(Boolean);
        return parts.length ? `账单配置：${parts.join(' · ')}` : '';
    };
    const resourceStatus = (value) => String(value || '').toLowerCase() === 'in_use' ? 'InUse' : String(value || '');
    const usagePresentation = (item) => {
        const itemKind = kind(item);
        const usage = metric(item);
        const resource = item?.current_resource || {};
        const disk = resource.system_disk;
        const eip = resource.eip;
        const config = configSummary(item, itemKind);
        const result = { primary: '', current: '', billing: '', config };
        if (itemKind === 'traffic') {
            result.primary = usage ? `账单出方向流量：${usage}` : '账单出方向流量未返回';
        } else if (itemKind === 'system-disk') {
            result.primary = numeric(disk?.size_gib) > 0 ? `当前系统盘：${number(disk.size_gib)} GiB` : '系统盘当前配置未返回';
            result.current = [disk?.category, resourceStatus(disk?.status)].filter(Boolean).join(' · ');
            if (result.current) result.current = `当前配置：${result.current}`;
            result.billing = usage ? (capacityTimeUnit(item?.unit) ? `账单累计容量时长：${usage}` : `账单累计用量：${usage}（不代表系统盘容量）`) : '账单用量未返回';
        } else if (itemKind === 'eip-holding') {
            const count = numeric(eip?.count);
            result.primary = count > 0 ? `当前 EIP：${number(count)} 个` : '公网 IP 保有服务（当前数量未返回）';
            result.current = [resourceStatus(eip?.status), numeric(eip?.bandwidth_mbps) > 0 ? `${number(eip.bandwidth_mbps)} Mbps` : ''].filter(Boolean).join(' · ');
            if (result.current) result.current = `当前配置：${result.current}`;
            const hours = serviceHours(item) ?? timeHours(item?.usage, item?.unit);
            result.billing = hours != null ? `账单保有时长：${number(hours)} 小时` : usage ? `账单累计用量：${usage}（不代表当前 IP 数量）` : '账单保有时长未返回';
        } else if (itemKind === 'instance-configuration' || itemKind === 'imageos') {
            const hours = timeHours(item?.usage, item?.unit) ?? serviceHours(item);
            result.primary = hours != null ? `账单计费时长：${number(hours)} 小时` : usage ? `账单累计用量：${usage}` : '账单用量未返回';
            result.billing = hours != null ? '计费用量，不代表实例实际运行时长。' : '未确认时间单位，不将累计用量换算为运行时长。';
        } else if (itemKind === 'snapshot') {
            result.primary = usage ? `快照账单用量：${usage}` : '快照账单用量未返回';
            result.billing = '账号账单中的快照用量，不代表当前实例的快照数量或容量。';
        } else {
            result.primary = usage ? `账单累计用量：${usage}` : '账单用量未返回';
            result.billing = usage ? '累计计费用量，不代表当前资源数量或容量。' : '';
        }
        return result;
    };

    return { number, amount, kind, itemLabels, usagePresentation, timeHours };
});
