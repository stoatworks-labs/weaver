'use strict';
'require view';
'require poll';
'require rpc';
'require dom';

// Live tunnel status from `weaver status --json`, through the luci.weaver
// rpcd plugin. Written for LuCI 21.02 (ES5, no newer client APIs).

var callStatus = rpc.declare({
	object: 'luci.weaver',
	method: 'status',
	expect: { '': {} }
});

var SVGNS = 'http://www.w3.org/2000/svg';
var HISTORY = 60;          // samples kept per path, one per poll (2 s)
var TX_COLOUR = '#2e7d32';
var RX_COLOUR = '#1565c0';
var STATE_COLOURS = {
	up: '#2e7d32',
	suspect: '#ef6c00',
	handshaking: '#ef6c00',
	down: '#c62828'
};

// "<peer>/<path id>" -> [ { tx: kbit/s, rx: kbit/s }, ... ]
var history = {};

function isNum(v) {
	return typeof(v) == 'number' && isFinite(v);
}

function fmtNum(v, digits, unit) {
	return isNum(v) ? v.toFixed(digits) + (unit ? ' ' + unit : '') : '-';
}

function fmtRate(kbps) {
	if (!isNum(kbps))
		return '-';
	if (kbps >= 1000)
		return (kbps / 1000).toFixed(kbps >= 100000 ? 0 : 1) + ' Mbit/s';
	return kbps.toFixed(0) + ' kbit/s';
}

function fmtBytes(v) {
	return isNum(v) ? '%1024.2mB'.format(v) : '-';
}

function fmtDuration(s) {
	return isNum(s) ? '%t'.format(s) : '-';
}

function fmtAgo(ms) {
	if (!isNum(ms))
		return '-';
	if (ms < 0)
		return _('never');
	if (ms < 1000)
		return _('%d ms ago').format(ms);
	return _('%s ago').format('%t'.format(Math.round(ms / 1000)));
}

function badge(state) {
	return E('span', {
		'style': 'display:inline-block;padding:0 .5em;border-radius:3px;' +
			'color:#fff;font-weight:bold;background:' +
			(STATE_COLOURS[state] || '#757575')
	}, [ state || '?' ]);
}

function svgEl(tag, attrs) {
	var el = document.createElementNS(SVGNS, tag);
	for (var k in attrs)
		if (attrs.hasOwnProperty(k))
			el.setAttribute(k, attrs[k]);
	return el;
}

// Throughput over the last HISTORY polls: tx and rx on one scale.
function sparkline(samples) {
	var w = 120, h = 28, max = 1, i;

	for (i = 0; i < samples.length; i++)
		max = Math.max(max, samples[i].tx, samples[i].rx);

	var points = function(key) {
		var pts = [];
		for (var j = 0; j < samples.length; j++) {
			var x = (HISTORY - samples.length + j) * (w - 1) / (HISTORY - 1);
			var y = (h - 1) - (samples[j][key] / max) * (h - 3);
			pts.push(x.toFixed(1) + ',' + y.toFixed(1));
		}
		return pts.join(' ');
	};

	var root = svgEl('svg', {
		'width': w,
		'height': h,
		'viewBox': '0 0 ' + w + ' ' + h,
		'style': 'vertical-align:middle'
	});
	var title = svgEl('title', {});
	title.appendChild(document.createTextNode(
		_('Last %d s, peak %s (tx green, rx blue)').format(HISTORY * 2, fmtRate(max))));
	root.appendChild(title);
	root.appendChild(svgEl('line', {
		'x1': 0, 'y1': h - 0.5, 'x2': w, 'y2': h - 0.5,
		'stroke': '#9e9e9e', 'stroke-width': 1
	}));
	if (samples.length > 1) {
		root.appendChild(svgEl('polyline', {
			'points': points('rx'), 'fill': 'none',
			'stroke': RX_COLOUR, 'stroke-width': 1.5
		}));
		root.appendChild(svgEl('polyline', {
			'points': points('tx'), 'fill': 'none',
			'stroke': TX_COLOUR, 'stroke-width': 1.5
		}));
	}
	return root;
}

function recordHistory(status) {
	var seen = {};

	(status.sessions || []).forEach(function(s) {
		(s.paths || []).forEach(function(p) {
			var key = (s.peer || s.public_key || '') + '/' + (p.id != null ? p.id : p.name);
			var h = history[key] || (history[key] = []);

			h.push({
				tx: isNum(p.tx_kbps) ? p.tx_kbps : 0,
				rx: isNum(p.rx_kbps) ? p.rx_kbps : 0
			});
			if (h.length > HISTORY)
				h.splice(0, h.length - HISTORY);
			seen[key] = true;
		});
	});

	for (var k in history)
		if (history.hasOwnProperty(k) && !seen[k])
			delete history[k];
}

// A two-column key/value table, as LuCI's own status pages use.
function infoTable(rows) {
	return E('table', { 'class': 'table' }, rows.map(function(r) {
		return E('tr', { 'class': 'tr' }, [
			E('td', { 'class': 'td left', 'width': '33%' }, [ r[0] ]),
			E('td', { 'class': 'td left' }, [ r[1] ])
		]);
	}));
}

function cell(title, content) {
	return E('td', { 'class': 'td', 'data-title': title },
		(content instanceof Node) ? content : [ content ]);
}

function pathTable(session) {
	var titles = [
		_('Path'), _('Priority'), _('State'), _('RTT'), _('RTT var.'),
		_('Loss'), _('Rate est.'), _('TX'), _('RX'), _('Last RX'),
		_('Throughput')
	];
	var rows = [
		E('tr', { 'class': 'tr table-titles' }, titles.map(function(t) {
			return E('th', { 'class': 'th' }, [ t ]);
		}))
	];
	var paths = session.paths || [];

	paths.forEach(function(p) {
		var key = (session.peer || session.public_key || '') + '/' + (p.id != null ? p.id : p.name);
		var name = p.name || ('#' + p.id);

		rows.push(E('tr', { 'class': 'tr' }, [
			cell(titles[0], E('span', {}, [
				E('strong', {}, [ name ]),
				p.device ? E('small', {}, [ ' (' + p.device + ')' ]) : E('span'),
				p.remote ? E('br') : E('span'),
				p.remote ? E('small', {}, [ p.remote ]) : E('span')
			])),
			cell(titles[1], (p.priority || '-') + (p.metered ? ' (' + _('metered') + ')' : '')),
			cell(titles[2], badge(p.state)),
			cell(titles[3], fmtNum(p.rtt_ms, 1, 'ms')),
			cell(titles[4], fmtNum(p.rttvar_ms, 1, 'ms')),
			cell(titles[5], fmtNum(p.loss_pct, 1, '%')),
			cell(titles[6], fmtRate(p.rate_kbps)),
			cell(titles[7], fmtRate(p.tx_kbps)),
			cell(titles[8], fmtRate(p.rx_kbps)),
			cell(titles[9], fmtAgo(p.last_rx_ms)),
			cell(titles[10], sparkline(history[key] || []))
		]));
	});

	if (!paths.length)
		rows.push(E('tr', { 'class': 'tr placeholder' }, [
			E('td', { 'class': 'td', 'colspan': titles.length }, [
				E('em', {}, [ _('No paths.') ])
			])
		]));

	return E('table', { 'class': 'table' }, rows);
}

function renderSession(s) {
	var r = s.reorder || {};

	return E('div', { 'class': 'cbi-section' }, [
		E('h3', {}, [ _('Session: %s').format(s.peer || s.public_key || '?') ]),
		infoTable([
			[ _('State'), badge(s.state) ],
			[ _('Endpoint'), s.endpoint || '-' ],
			[ _('Established'), (s.state == 'up') ? fmtDuration(s.established_s) : '-' ],
			[ _('Last handshake'), isNum(s.last_handshake_s) ? _('%s ago').format('%t'.format(s.last_handshake_s)) : '-' ],
			[ _('Transferred'), _('%s sent, %s received').format(fmtBytes(s.tx_bytes), fmtBytes(s.rx_bytes)) ],
			[ _('Reordering'), _('hold %s; %d held, %d timeouts, %d duplicates, %d late').format(
				fmtNum(r.hold_ms, 1, 'ms'), r.held || 0, r.timeouts || 0, r.duplicates || 0, r.late || 0) ]
		]),
		pathTable(s)
	]);
}

function renderNotRunning(status, rpcError) {
	var why;

	if (rpcError)
		why = _('The status call failed: %s').format(rpcError);
	else if (status.enabled === false)
		why = _('It is disabled. Enable it on the Settings tab.');
	else
		why = _('It is enabled but not running. "logread -e weaver" on the router shows why.');

	return E('div', { 'class': 'cbi-section' }, [
		E('p', {}, [ E('strong', {}, [ _('weaver is not running.') ]), ' ', why ]),
		(status && status.detail) ? E('pre', { 'style': 'white-space:pre-wrap' }, [ status.detail ]) : E('span')
	]);
}

function renderStatus(status, rpcError) {
	if (rpcError || !status || status.error)
		return [ renderNotRunning(status || {}, rpcError) ];

	var tun = status.tun || {};
	var sessions = status.sessions || [];
	var nodes = [
		E('div', { 'class': 'cbi-section' }, [
			infoTable([
				[ _('Version'), status.version || '-' ],
				[ _('Role'), status.role || '-' ],
				[ _('Mode'), status.mode || '-' ],
				[ _('Uptime'), fmtDuration(status.uptime_s) ],
				[ _('Public key'), status.public_key || '-' ],
				[ _('Tunnel'), [ tun.name, tun.address, tun.mtu ? 'MTU ' + tun.mtu : null ]
					.filter(function(x) { return x; }).join(', ') || '-' ]
			])
		])
	];

	if (!sessions.length)
		nodes.push(E('div', { 'class': 'cbi-section' }, [
			E('p', {}, [ E('em', {}, [ status.role == 'server'
				? _('No client has connected yet.')
				: _('No session yet.') ]) ])
		]));

	sessions.forEach(function(s) {
		nodes.push(renderSession(s));
	});

	return nodes;
}

return view.extend({
	load: function() {
		return callStatus().catch(function(err) {
			return { error: 'rpc', rpcError: String(err) };
		});
	},

	update: function(container, status) {
		if (status && !status.error)
			recordHistory(status);
		dom.content(container, renderStatus(status, status ? status.rpcError : null));
	},

	render: function(status) {
		var container = E('div', {});

		this.update(container, status);

		poll.add(L.bind(function() {
			return callStatus().then(L.bind(function(st) {
				this.update(container, st);
			}, this)).catch(L.bind(function(err) {
				this.update(container, { error: 'rpc', rpcError: String(err) });
			}, this));
		}, this), 2);

		return E('div', {}, [
			E('h2', {}, [ _('Weaver') ]),
			E('div', { 'class': 'cbi-map-descr' }, [
				_('Live state of the bonded tunnel, refreshed every 2 seconds.')
			]),
			container
		]);
	},

	handleSaveApply: null,
	handleSave: null,
	handleReset: null
});
