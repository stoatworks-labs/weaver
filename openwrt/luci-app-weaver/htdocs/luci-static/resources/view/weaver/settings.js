'use strict';
'require view';
'require form';
'require rpc';
'require uci';
'require ui';
'require dom';
'require tools.widgets as widgets';

// /etc/config/weaver. Written for LuCI 21.02 (ES5, form API of that release).

var callPubkey = rpc.declare({
	object: 'luci.weaver',
	method: 'pubkey',
	expect: { '': {} }
});

var callGenkey = rpc.declare({
	object: 'luci.weaver',
	method: 'genkey',
	expect: { '': {} }
});

// A Curve25519 key in base64: 32 bytes, 43 characters and one "=".
function validateKey(section_id, value) {
	if (value == null || value === '')
		return true;
	if (/^[A-Za-z0-9+\/]{43}=$/.test(value))
		return true;
	return _('Expecting a base64 key: 44 characters ending in "="');
}

function shortKey(value) {
	return value ? value.substr(0, 10) + '…' : '-';
}

// Modal-only control in the peer editor: makes a key pair, puts the public
// half in the peer's public_key field and shows the private half once, with
// the commands that set up the client router. Nothing secret is saved here.
var KeyPairValue = form.DummyValue.extend({
	serverPublicKey: null,

	renderWidget: function(section_id) {
		var output = E('div', {});
		var button = E('button', {
			'type': 'button',
			'class': 'cbi-button cbi-button-action',
			'click': ui.createHandlerFn(this, 'handleGenerate', section_id, output)
		}, [ _('Generate key pair') ]);

		return E('div', {}, [ button, output ]);
	},

	handleGenerate: function(section_id, output) {
		return callGenkey().then(L.bind(function(res) {
			if (!res || !res.private_key || !res.public_key) {
				dom.content(output, E('p', { 'class': 'alert-message warning' }, [
					_('Could not generate a key pair: %s').format((res && res.error) || _('no reply'))
				]));
				return;
			}

			// setValue() alone leaves the field marked invalid (it was empty
			// and required) until the user types in it.
			var pkField = this.map.lookupOption('public_key', section_id);
			var pkWidget = pkField ? pkField[0].getUIElement(section_id) : null;
			if (pkWidget) {
				pkWidget.setValue(res.public_key);
				pkWidget.triggerValidation();
			}

			var addrField = this.map.lookupOption('address', section_id);
			var addr = addrField ? addrField[0].formvalue(section_id) : '';
			var own = uci.get('weaver', 'main', 'address') || '10.77.0.1/24';
			var prefix = own.indexOf('/') > -1 ? own.substr(own.indexOf('/')) : '/24';
			var port = uci.get('weaver', 'main', 'listen_port') || '9328';
			var snippet = [
				"uci set weaver.main.role='client'",
				"uci set weaver.main.private_key='" + res.private_key + "'",
				"uci set weaver.main.server='<this router’s public address>:" + port + "'",
				"uci set weaver.main.server_public_key='" + (this.serverPublicKey || '<this router’s public key>') + "'",
				"uci set weaver.main.address='" + (addr ? addr + prefix : '<peer address>' + prefix) + "'",
				"uci set weaver.main.enabled='1'",
				'uci commit weaver'
			].join('\n');

			dom.content(output, [
				E('p', {}, [
					_('The public key is filled in above. The private key below is shown only now and is not stored on this router: copy it to the client router, then save this peer.')
				]),
				E('p', {}, [ E('strong', {}, [ _('Client private key') ]) ]),
				E('input', {
					'type': 'text', 'readonly': 'readonly', 'class': 'cbi-input-text',
					'style': 'width:100%;font-family:monospace', 'value': res.private_key,
					'click': function(ev) { ev.target.select(); }
				}),
				E('p', {}, [ E('strong', {}, [ _('On the client router (SSH)') ]) ]),
				E('textarea', {
					'readonly': 'readonly', 'rows': 8, 'wrap': 'off',
					'style': 'width:100%;font-family:monospace',
					'click': function(ev) { ev.target.select(); }
				}, [ snippet ])
			]);
		}, this));
	}
});

return view.extend({
	load: function() {
		return Promise.all([
			callPubkey().catch(function(err) { return { error: String(err) }; }),
			uci.load('weaver')
		]);
	},

	render: function(data) {
		var pk = data[0] || {};
		var m, s, o;

		m = new form.Map('weaver', _('Weaver'),
			_('Bond this router’s WAN links into one encrypted tunnel. A client bonds its WANs to one server; a server accepts clients listed under Peers.'));

		/* main ------------------------------------------------------------ */

		s = m.section(form.NamedSection, 'main', 'weaver', _('Tunnel'));
		s.addremove = false;
		s.tab('general', _('General Settings'));
		s.tab('routing', _('Routing'));
		s.tab('advanced', _('Advanced Settings'));

		o = s.taboption('general', form.Flag, 'enabled', _('Enable'));
		o.rmempty = false;

		o = s.taboption('general', form.ListValue, 'role', _('Role'));
		o.value('client', _('Client: bond this router’s WANs to a server'));
		o.value('server', _('Server: accept client routers'));
		o.default = 'client';

		o = s.taboption('general', form.DummyValue, '_public_key', _('Public key'),
			_('Give this to the other end: a client needs its server’s key, a server needs each client’s. Derived from the private key on the Advanced tab.'));
		o.cfgvalue = function() {
			return pk.public_key || _('Unavailable (%s)').format(pk.error || _('no private key'));
		};

		o = s.taboption('general', form.Value, 'server', _('Server'),
			_('Host name or address of the server, with its port.'));
		o.depends('role', 'client');
		o.datatype = 'hostport';
		o.placeholder = 'vps.example.net:9328';
		o.rmempty = false;

		o = s.taboption('general', form.Value, 'server_public_key', _('Server public key'));
		o.depends('role', 'client');
		o.validate = validateKey;
		o.rmempty = false;

		o = s.taboption('general', form.Value, 'listen_port', _('Listen port'),
			_('UDP port clients connect to. The Allow-Weaver firewall rule opens it from wan while weaver runs as a server.'));
		o.depends('role', 'server');
		o.datatype = 'port';
		o.placeholder = '9328';

		o = s.taboption('general', form.Value, 'address', _('Tunnel address'),
			_('This end’s address on weaver0, with the prefix. Defaults: 10.77.0.2/24 for a client, 10.77.0.1/24 for a server.'));
		o.datatype = 'cidr4';
		o.placeholder = '10.77.0.2/24';

		o = s.taboption('general', form.ListValue, 'mode', _('Mode'),
			_('Adaptive duplicates real-time traffic (calls, games, DNS) on the two best paths and bonds the rest. Bond splits everything. Redundant sends every packet on every path. Failover uses one path at a time.'));
		o.value('adaptive', _('Adaptive'));
		o.value('bond', _('Bond'));
		o.value('redundant', _('Redundant'));
		o.value('failover', _('Failover'));
		o.default = 'adaptive';

		o = s.taboption('routing', form.Flag, 'auto_paths', _('Use all WANs automatically'),
			_('Every up interface with an IPv4 default route becomes a primary path unless it has its own entry under Paths.'));
		o.depends('role', 'client');
		o.default = '1';
		o.rmempty = false;

		o = s.taboption('routing', form.Flag, 'default_route', _('Route all traffic through the tunnel'),
			_('Adds 0.0.0.0/1 and 128.0.0.0/1 via weaver0. The WAN default routes stay, so disabling weaver restores direct routing.'));
		o.depends('role', 'client');
		o.default = '1';
		o.rmempty = false;

		o = s.taboption('routing', form.DynamicList, 'subnet', _('Tunnelled subnets'),
			_('Remote subnets to route through the tunnel, in addition to or instead of the default route.'));
		o.depends('role', 'client');
		o.datatype = 'cidr4';

		o = s.taboption('routing', form.Flag, 'fallback_direct', _('Fall back to direct'),
			_('When every path has been down for 5 seconds, remove the tunnel routes so the LAN reaches the internet directly; put them back when the tunnel returns.'));
		o.depends('role', 'client');
		o.default = '1';
		o.rmempty = false;

		o = s.taboption('advanced', form.Value, 'private_key', _('Private key'),
			_('Generated on install. Changing it changes the public key, and every other end must then be told the new one.'));
		o.password = true;
		o.validate = validateKey;
		o.rmempty = false;

		o = s.taboption('advanced', form.Value, 'mtu', _('MTU'));
		o.datatype = 'range(576,9000)';
		o.placeholder = '1380';

		o = s.taboption('advanced', form.Value, 'probe_interval', _('Probe interval (ms)'),
			_('How often each path is probed while traffic flows.'));
		o.depends('role', 'client');
		o.datatype = 'range(20,10000)';
		o.placeholder = '200';

		o = s.taboption('advanced', form.Value, 'path_timeout', _('Path timeout (ms)'),
			_('A path that has received nothing for this long is down.'));
		o.datatype = 'range(100,60000)';
		o.placeholder = '1000';

		o = s.taboption('advanced', form.ListValue, 'log_level', _('Log level'));
		o.value('debug', _('Debug'));
		o.value('info', _('Info'));
		o.value('warn', _('Warning'));
		o.value('error', _('Error'));
		o.default = 'info';

		/* paths ----------------------------------------------------------- */

		s = m.section(form.GridSection, 'path', _('Paths'),
			_('Client only. Pins an interface’s priority or gives it rate hints; "disabled" keeps it out of the bond even with automatic WANs on. Primary paths are always used, secondary ones take overflow, backup ones carry traffic only when nothing better is up.'));
		s.anonymous = true;
		s.addremove = true;
		s.sortable = true;
		s.nodescriptions = true;

		o = s.option(widgets.NetworkSelect, 'interface', _('Interface'));
		o.nocreate = true;
		o.exclude = 'weaver';
		o.rmempty = false;

		o = s.option(form.ListValue, 'priority', _('Priority'));
		o.value('primary', _('Primary'));
		o.value('secondary', _('Secondary'));
		o.value('backup', _('Backup'));
		o.value('disabled', _('Disabled'));
		o.default = 'primary';

		o = s.option(form.Value, 'up_kbps', _('Upload (kbit/s)'),
			_('The link’s upload speed if known; empty or 0 lets weaver measure it.'));
		o.datatype = 'uinteger';
		o.placeholder = _('auto');

		o = s.option(form.Value, 'down_kbps', _('Download (kbit/s)'));
		o.datatype = 'uinteger';
		o.placeholder = _('auto');

		o = s.option(form.Flag, 'metered', _('Metered'),
			_('Probe less often while idle, to save data on a cellular link.'));
		o.default = '0';

		/* peers ----------------------------------------------------------- */

		s = m.section(form.GridSection, 'peer', _('Peers'),
			_('Server only. One entry per client router: its public key, its tunnel address, and the subnets behind it the server may route to.'));
		s.anonymous = true;
		s.addremove = true;
		s.nodescriptions = true;
		s.modaltitle = _('Peer');

		o = s.option(form.Value, 'name', _('Name'));
		o.rmempty = false;
		o.placeholder = 'flint';

		o = s.option(form.Value, 'public_key', _('Public key'));
		o.validate = validateKey;
		o.rmempty = false;
		o.textvalue = function(section_id) {
			return shortKey(this.cfgvalue(section_id));
		};

		o = s.option(form.Value, 'address', _('Tunnel address'),
			_('The client’s address on weaver0, without a prefix.'));
		o.datatype = 'ip4addr(1)';
		o.placeholder = '10.77.0.2';
		o.rmempty = false;

		o = s.option(form.DynamicList, 'allowed_ip', _('Allowed IPs'),
			_('Subnets behind this client, e.g. its LAN. Not needed when the client masquerades (the default).'));
		o.datatype = 'cidr4';

		o = s.option(KeyPairValue, '_keypair', _('Key pair'),
			_('For a new client: generate its keys here and paste the private key into the client router.'));
		o.modalonly = true;
		o.serverPublicKey = pk.public_key || null;

		return m.render();
	}
});
