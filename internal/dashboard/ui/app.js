function karmax() {
    return {
        tab: 'dashboard',
        connected: false,
        agents: [],
        jobs: [],
        events: [],
        liveEvents: [],
        webhookRoutes: [],
        webhookEvents: [],
        tools: [],
        selectedAgent: null,
        triggerPayload: '{}',
        ws: null,

        async init() {
            await this.refresh();
            this.connectWS();
            setInterval(() => this.refresh(), 5000);
        },

        async refresh() {
            try {
                const [agents, jobs, events, routes, whEvents, tools] = await Promise.all([
                    this.api('/api/agents'),
                    this.api('/api/scheduler/jobs'),
                    this.api('/api/events?limit=50'),
                    this.api('/api/webhooks/routes'),
                    this.api('/api/webhooks/events?limit=20'),
                    this.api('/api/tools'),
                ]);
                this.agents = agents || [];
                this.jobs = jobs || [];
                this.events = events || [];
                this.webhookRoutes = routes || [];
                this.webhookEvents = whEvents || [];
                this.tools = tools || [];
            } catch (e) {
                console.error('refresh error:', e);
            }
        },

        connectWS() {
            const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
            this.ws = new WebSocket(`${proto}//${location.host}/ws`);

            this.ws.onopen = () => { this.connected = true; };
            this.ws.onclose = () => {
                this.connected = false;
                setTimeout(() => this.connectWS(), 3000);
            };
            this.ws.onmessage = (e) => {
                try {
                    const event = JSON.parse(e.data);
                    this.liveEvents.push(event);
                    if (this.liveEvents.length > 200) {
                        this.liveEvents = this.liveEvents.slice(-100);
                    }
                } catch (err) {}
            };
        },

        async api(path, opts = {}) {
            const resp = await fetch(path, {
                headers: { 'Content-Type': 'application/json' },
                ...opts,
            });
            if (!resp.ok) throw new Error(resp.statusText);
            return resp.json();
        },

        async agentAction(id, action) {
            try {
                await this.api(`/api/agents/${id}/${action}`, { method: 'POST' });
                await this.refresh();
            } catch (e) {
                console.error(`agent ${action} error:`, e);
            }
        },

        async triggerAgent(id) {
            try {
                let payload = {};
                try { payload = JSON.parse(this.triggerPayload); } catch {}
                await this.api(`/api/agents/${id}/trigger`, {
                    method: 'POST',
                    body: JSON.stringify(payload),
                });
            } catch (e) {
                console.error('trigger error:', e);
            }
        },

        async runJob(id) {
            try {
                await this.api(`/api/scheduler/jobs/${id}/run`, { method: 'POST' });
            } catch (e) {
                console.error('run job error:', e);
            }
        },

        async deleteJob(id) {
            if (!confirm('Delete this job?')) return;
            try {
                await this.api(`/api/scheduler/jobs/${id}`, { method: 'DELETE' });
                await this.refresh();
            } catch (e) {
                console.error('delete job error:', e);
            }
        },

        formatTime(ts) {
            if (!ts) return '';
            const d = new Date(ts);
            return d.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' });
        }
    };
}
