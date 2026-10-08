<script setup lang="ts">
import { nextTick, ref } from 'vue'
import { conversationApi, type ConversationMessageDTO, type ConversationReplyDTO, type ConversationSessionDTO } from '@/api'
import { sessionStateLabel } from '@/utils/session'
import PageHeader from '@/components/PageHeader.vue'

const currentUserID = ref('operator')
const sessions = ref<ConversationSessionDTO[]>([])
const activeSessionID = ref('')
const messages = ref<ConversationMessageDTO[]>([])
const input = ref('')
const loading = ref(false)
const error = ref('')

function findSession(id: string): ConversationSessionDTO | undefined {
	return sessions.value.find(s => s.id === id)
}

async function refreshSessions() {
	try {
		sessions.value = await conversationApi.listSessions(currentUserID.value)
	} catch (e) {
		error.value = e instanceof Error ? e.message : String(e)
	}
}

async function newSession() {
	try {
		error.value = ''
		const sess = await conversationApi.newSession(currentUserID.value)
		sessions.value = [sess, ...sessions.value]
		await openSession(sess.id)
	} catch (e) {
		error.value = e instanceof Error ? e.message : String(e)
	}
}

async function openSession(id: string) {
	activeSessionID.value = id
	const sess = findSession(id)
	messages.value = sess ? [...sess.messages] : []
}

async function refreshActiveSession() {
	if (!activeSessionID.value) return
	try {
		const sess = await conversationApi.getSession(activeSessionID.value, currentUserID.value)
		messages.value = [...sess.messages]
		// Update the session entry in the list (state may have changed).
		const idx = sessions.value.findIndex(s => s.id === sess.id)
		if (idx >= 0) sessions.value[idx] = sess
	} catch (e) {
		error.value = e instanceof Error ? e.message : String(e)
	}
}

async function send() {
	const text = input.value.trim()
	if (!text || !activeSessionID.value || loading.value) return
	input.value = ''
	messages.value.push({ id: `local-${Date.now()}`, role: 'user', content: text, timestamp: new Date().toISOString() })
	await scrollToBottom()
	try {
		loading.value = true
		const reply: ConversationReplyDTO = await conversationApi.sendMessage(activeSessionID.value, currentUserID.value, text)
		messages.value.push({
			id: reply.session_id || `reply-${Date.now()}`,
			role: 'assistant',
			content: reply.text,
			timestamp: new Date().toISOString(),
			action: reply.action_type ? { type: reply.action_type, payload: reply.action_payload } : undefined,
		})
		await scrollToBottom()
		// Refresh to reflect any state change (e.g. reviewing -> done).
		await refreshActiveSession()
		await refreshSessions()
	} catch (e) {
		error.value = e instanceof Error ? e.message : String(e)
	} finally {
		loading.value = false
	}
}

async function closeSession(id: string) {
	try {
		await conversationApi.closeSession(id, currentUserID.value)
		sessions.value = sessions.value.filter(s => s.id !== id)
		if (activeSessionID.value === id) {
			activeSessionID.value = ''
			messages.value = []
		}
	} catch (e) {
		error.value = e instanceof Error ? e.message : String(e)
	}
}

let scrollEl: HTMLElement | null = null
async function scrollToBottom() {
	await nextTick()
	if (scrollEl) scrollEl.scrollTop = scrollEl.scrollHeight
}

function formatTime(iso: string): string {
	const t = new Date(iso)
	if (Number.isNaN(t.getTime())) return ''
	return t.toLocaleTimeString()
}

function onContainer(el: any) {
	scrollEl = el as HTMLElement
}

// Load sessions on mount.
refreshSessions()
</script>

<template>
	<div class="levee-page">
		<PageHeader title="AI 对话运维" description="用自然语言发起变更、查询状态与诊断（会话按操作员隔离）">
			<template #actions>
				<el-button :icon="'Refresh'" size="small" @click="refreshSessions">刷新会话</el-button>
			</template>
		</PageHeader>

		<div class="chat-layout">
			<!-- Session list -->
			<aside class="lv-panel sessions">
				<div class="lv-panel__head">
					<span class="lv-panel__title">会话</span>
					<el-button type="primary" size="small" :icon="'Plus'" @click="newSession">新建</el-button>
				</div>
				<div class="sessions__list">
					<div v-if="sessions.length === 0" class="lv-empty">
						<span>暂无会话</span>
						<span>点「新建」开始一次对话。</span>
					</div>
					<button
						v-for="s in sessions"
						:key="s.id"
						type="button"
						class="session"
						:class="{ 'session--active': s.id === activeSessionID }"
						@click="openSession(s.id)"
					>
						<div class="session__top">
							<span class="session__id lv-mono">{{ s.id.slice(0, 8) }}…</span>
							<span class="session__state">{{ sessionStateLabel(s.state) }}</span>
						</div>
						<div class="session__meta">
							{{ s.messages.length }} 条消息 · {{ formatTime(s.updated_at) }}
						</div>
						<span class="session__close" title="关闭会话" @click.stop="closeSession(s.id)">
							<el-icon><Close /></el-icon>
						</span>
					</button>
				</div>
			</aside>

			<!-- Conversation -->
			<section class="lv-panel chat">
				<div class="lv-panel__head">
					<span class="lv-panel__title">
						{{ activeSessionID ? `会话 ${activeSessionID.slice(0, 8)}…` : '对话' }}
					</span>
					<span v-if="activeSessionID" class="lv-panel__hint lv-mono">{{ messages.length }} 条</span>
				</div>

				<div v-if="!activeSessionID" class="lv-empty chat__empty">
					<el-icon class="lv-empty__icon"><ChatDotRound /></el-icon>
					<span class="lv-empty__title">未选择会话</span>
					<span>从左侧选择，或新建一个会话开始。</span>
				</div>

				<template v-else>
					<div class="messages" :ref="onContainer">
						<div v-if="messages.length === 0" class="lv-empty">
							<span>还没有消息，试试 /help 查看可用命令。</span>
						</div>
						<div v-for="m in messages" :key="m.id" class="msg" :class="`msg--${m.role}`">
							<div class="msg__role">
								{{ m.role === 'user' ? '操作员' : m.role === 'assistant' ? 'LEVEE' : '系统' }}
								<span class="msg__time lv-mono">{{ formatTime(m.timestamp) }}</span>
							</div>
							<div class="msg__bubble">
								{{ m.content }}
								<span v-if="m.action" class="msg__action lv-mono">{{ m.action.type }}</span>
							</div>
						</div>
					</div>

					<div class="composer">
						<el-input
							v-model="input"
							type="textarea"
							:rows="2"
							placeholder="输入消息，Enter 发送（Shift+Enter 换行）；试试 /help"
							@keyup.enter.exact.prevent="send"
						/>
						<el-button type="primary" :loading="loading" @click="send">发送</el-button>
					</div>
				</template>
			</section>
		</div>

		<p v-if="error" class="error">{{ error }}</p>
	</div>
</template>

<style scoped>
.chat-layout {
	display: grid;
	grid-template-columns: 264px minmax(0, 1fr);
	gap: var(--lv-space-4);
	align-items: start;
}

@media (max-width: 900px) {
	.chat-layout {
		grid-template-columns: minmax(0, 1fr);
	}
}

/* ---------------------------------------------------------------- sessions */

.sessions {
	overflow: hidden;
}

.sessions__list {
	max-height: 62vh;
	overflow-y: auto;
	padding: var(--lv-space-2);
}

.session {
	position: relative;
	display: block;
	width: 100%;
	padding: var(--lv-space-3);
	margin-bottom: 4px;
	border: 1px solid transparent;
	border-radius: var(--lv-radius);
	background: transparent;
	font-family: inherit;
	text-align: left;
	cursor: pointer;
	transition:
		background-color var(--lv-dur-fast) var(--lv-ease),
		border-color var(--lv-dur-fast) var(--lv-ease);
}

.session:hover {
	background: var(--lv-surface-hover);
}

.session--active {
	background: var(--lv-accent-soft);
	border-color: var(--lv-accent-border);
}

.session__top {
	display: flex;
	align-items: center;
	justify-content: space-between;
	gap: var(--lv-space-2);
}

.session__id {
	font-size: var(--lv-text-sm);
	font-weight: 600;
	color: var(--lv-text-1);
}

.session__state {
	font-size: var(--lv-text-xs);
	color: var(--lv-text-3);
}

.session--active .session__state {
	color: var(--lv-accent);
}

.session__meta {
	margin-top: 2px;
	font-size: 11px;
	color: var(--lv-text-3);
}

.session__close {
	position: absolute;
	right: 6px;
	bottom: 6px;
	display: none;
	align-items: center;
	justify-content: center;
	width: 20px;
	height: 20px;
	border-radius: var(--lv-radius-sm);
	color: var(--lv-text-3);
}

.session:hover .session__close {
	display: inline-flex;
}

.session__close:hover {
	background: var(--lv-bad-soft);
	color: var(--lv-bad);
}

/* -------------------------------------------------------------------- chat */

.chat {
	display: flex;
	flex-direction: column;
	height: 68vh;
}

.chat__empty {
	flex: 1;
	justify-content: center;
}

.messages {
	flex: 1;
	overflow-y: auto;
	padding: var(--lv-space-4);
	display: flex;
	flex-direction: column;
	gap: var(--lv-space-4);
}

.msg {
	display: flex;
	flex-direction: column;
	gap: 4px;
	max-width: 78%;
}

.msg--user {
	align-self: flex-end;
	align-items: flex-end;
}

.msg--assistant,
.msg--system {
	align-self: flex-start;
	align-items: flex-start;
}

.msg__role {
	display: flex;
	align-items: baseline;
	gap: var(--lv-space-2);
	font-size: var(--lv-text-xs);
	color: var(--lv-text-3);
}

.msg__time {
	font-size: 10px;
	color: var(--lv-ink-400);
}

.msg__bubble {
	padding: var(--lv-space-3) var(--lv-space-4);
	border-radius: var(--lv-radius-lg);
	font-size: var(--lv-text-sm);
	line-height: 1.65;
	white-space: pre-wrap;
	word-break: break-word;
	border: 1px solid transparent;
}

.msg--user .msg__bubble {
	background: var(--lv-accent);
	color: var(--lv-accent-contrast);
}

.msg--assistant .msg__bubble {
	background: var(--lv-surface-3);
	border-color: var(--lv-border);
	color: var(--lv-text-1);
}

.msg--system .msg__bubble {
	background: var(--lv-warn-soft);
	border-color: var(--lv-warn-border);
	color: var(--lv-warn);
}

.msg__action {
	display: inline-block;
	margin-left: 6px;
	padding: 1px 6px;
	border-radius: var(--lv-radius-sm);
	background: rgba(0, 0, 0, 0.06);
	font-size: 11px;
}

/* ---------------------------------------------------------------- composer */

.composer {
	display: flex;
	align-items: flex-end;
	gap: var(--lv-space-2);
	padding: var(--lv-space-3) var(--lv-space-4);
	border-top: 1px solid var(--lv-border-soft);
}

.composer .el-input {
	flex: 1;
}

.error {
	margin-top: var(--lv-space-4);
	color: var(--lv-bad);
	font-size: var(--lv-text-sm);
}
</style>
