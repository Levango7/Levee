<script setup lang="ts">
import { nextTick, ref } from 'vue'
import { conversationApi, type ConversationMessageDTO, type ConversationReplyDTO, type ConversationSessionDTO } from '@/api'

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
		const sess = await conversationApi.getSession(activeSessionID.value)
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
		await conversationApi.closeSession(id)
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
	<div class="levee-page conversation">
		<h2 class="levee-page__title">AI 对话运维</h2>

		<div class="conversation__layout">
			<!-- Session list sidebar -->
			<aside class="conversation__sidebar">
				<div class="conversation__sidebar-header">
					<el-button type="primary" size="small" @click="newSession">+ 新建会话</el-button>
				</div>
				<div v-if="sessions.length === 0" class="conversation__empty">暂无会话，点击新建开始对话</div>
				<div
					v-for="s in sessions"
					:key="s.id"
					:class="['conversation__session-item', { active: s.id === activeSessionID }]"
					@click="openSession(s.id)"
				>
					<div class="conversation__session-top">
						<span class="conversation__session-id">{{ s.id.slice(0, 8) }}…</span>
						<el-tag size="small" :type="s.state === 'failed' ? 'danger' : s.state === 'done' ? 'success' : 'info'">
							{{ s.state }}
						</el-tag>
					</div>
					<div class="conversation__session-meta">
						{{ s.messages.length }} 条消息 · {{ formatTime(s.updated_at) }}
					</div>
					<el-button
						link
						type="danger"
						size="small"
						class="conversation__session-close"
						@click.stop="closeSession(s.id)"
					>关闭</el-button>
				</div>
			</aside>

			<!-- Chat area -->
			<main class="conversation__chat">
				<div v-if="!activeSessionID" class="conversation__empty conversation__empty--main">
					选择一个会话或新建会话开始对话
				</div>
				<template v-else>
					<div class="conversation__messages" :ref="onContainer">
						<div
							v-for="m in messages"
							:key="m.id"
							:class="['message', `message--${m.role}`]"
						>
							<div class="message__role">{{ m.role === 'user' ? '操作员' : m.role === 'assistant' ? 'LEVEE' : '系统' }}</div>
							<div class="message__bubble">
								{{ m.content }}
								<span v-if="m.action" class="message__action">[{{ m.action.type }}]</span>
							</div>
							<div class="message__time">{{ formatTime(m.timestamp) }}</div>
						</div>
					</div>
					<div class="conversation__input">
						<el-input
							v-model="input"
							type="textarea"
							:rows="2"
							placeholder="输入消息，试试 /help 查看命令"
							@keyup.enter.exact.prevent="send"
						/>
						<el-button type="primary" :loading="loading" @click="send">发送</el-button>
					</div>
				</template>
			</main>
		</div>

		<p v-if="error" class="error">{{ error }}</p>
	</div>
</template>

<style scoped>
.conversation__layout {
	display: flex;
	gap: 1rem;
	min-height: 60vh;
}
.conversation__sidebar {
	width: 18rem;
	border-right: 1px solid var(--el-border-color-lighter);
	padding-right: 1rem;
	overflow-y: auto;
	max-height: 70vh;
}
.conversation__sidebar-header {
	margin-bottom: 0.75rem;
}
.conversation__session-item {
	padding: 0.5rem;
	border-radius: 6px;
	cursor: pointer;
	margin-bottom: 0.5rem;
	border: 1px solid transparent;
}
.conversation__session-item:hover {
	background: var(--el-fill-color-light);
}
.conversation__session-item.active {
	border-color: var(--el-color-primary);
	background: var(--el-color-primary-light-9);
}
.conversation__session-top {
	display: flex;
	justify-content: space-between;
	align-items: center;
}
.conversation__session-id {
	font-weight: 600;
	font-family: monospace;
}
.conversation__session-meta {
	font-size: 0.75rem;
	color: var(--el-text-color-secondary);
	margin: 0.25rem 0;
}
.conversation__session-close {
	float: right;
}
.conversation__chat {
	flex: 1;
	display: flex;
	flex-direction: column;
}
.conversation__messages {
	flex: 1;
	overflow-y: auto;
	max-height: 60vh;
	padding: 0.5rem;
	border: 1px solid var(--el-border-color-lighter);
	border-radius: 6px;
	margin-bottom: 0.75rem;
}
.conversation__input {
	display: flex;
	gap: 0.5rem;
	align-items: flex-end;
}
.conversation__empty {
	color: var(--el-text-color-secondary);
	text-align: center;
	padding: 2rem;
}
.message {
	margin-bottom: 0.75rem;
	display: flex;
	flex-direction: column;
}
.message--user {
	align-items: flex-end;
}
.message--assistant,
.message--system {
	align-items: flex-start;
}
.message__role {
	font-size: 0.7rem;
	color: var(--el-text-color-secondary);
	margin-bottom: 0.15rem;
}
.message__bubble {
	padding: 0.5rem 0.75rem;
	border-radius: 8px;
	max-width: 80%;
	white-space: pre-wrap;
	word-break: break-word;
}
.message--user .message__bubble {
	background: var(--el-color-primary);
	color: #fff;
}
.message--assistant .message__bubble {
	background: var(--el-fill-color);
}
.message--system .message__bubble {
	background: var(--el-color-warning-light-9);
}
.message__time {
	font-size: 0.65rem;
	color: var(--el-text-color-secondary);
	margin-top: 0.1rem;
}
.message__action {
	font-size: 0.7rem;
	color: var(--el-color-warning);
}
.error {
	color: var(--el-color-danger);
	margin-top: 1rem;
}
</style>
