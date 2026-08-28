import { createRouter, createWebHistory, RouteRecordRaw } from 'vue-router'

// Route table. Each entry's `meta.title` is shown in the top bar by App.vue.
// We use lazy-loaded components so that the initial bundle only contains the
// shell + the active view; this keeps the embed payload small.
const routes: RouteRecordRaw[] = [
  {
    path: '/',
    redirect: '/changes',
  },
  {
    path: '/changes',
    name: 'changes',
    component: () => import('@/views/ChangesView.vue'),
    meta: { title: '变更看板' },
  },
  {
    path: '/changes/:id',
    name: 'change-detail',
    component: () => import('@/views/ChangesView.vue'),
    meta: { title: '变更详情' },
  },
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/LoginView.vue'),
    meta: { title: '登录' },
  },
  // OIDC authorization-code redirect target: the IdP sends the browser back
  // here with ?code=...&state=...; the view completes the token exchange.
  {
    path: '/login/callback',
    name: 'login-callback',
    component: () => import('@/views/LoginCallbackView.vue'),
    meta: { title: 'SSO 登录' },
  },
  {
    path: '/approval',
    name: 'approval',
    component: () => import('@/views/ApprovalView.vue'),
    meta: { title: '审批中心' },
  },
  {
    path: '/monitor',
    name: 'monitor',
    component: () => import('@/views/MonitorView.vue'),
    meta: { title: '实时监控' },
  },
  {
    path: '/monitor/:changeId',
    name: 'monitor-change',
    component: () => import('@/views/MonitorView.vue'),
    meta: { title: '执行监控' },
  },
  {
    path: '/templates',
    name: 'templates',
    component: () => import('@/views/TemplatesView.vue'),
    meta: { title: '模板管理' },
  },
  {
    path: '/targets',
    name: 'targets',
    component: () => import('@/views/TargetsView.vue'),
    meta: { title: '目标机管理' },
  },
  {
    path: '/audit',
    name: 'audit',
    component: () => import('@/views/AuditView.vue'),
    meta: { title: '审计查询' },
  },
  {
    path: '/system',
    name: 'system',
    component: () => import('@/views/SystemView.vue'),
    meta: { title: '系统状态' },
  },
  // Mobile approval deeplinks (push notifications link here):
  // /m/approve/<changeId>?token=... and /m/reject/<changeId>?token=...
  {
    path: '/m/approve/:id',
    name: 'mobile-approve',
    component: () => import('@/views/MobileApprovalView.vue'),
    meta: { title: '移动审批' },
  },
  {
    path: '/m/reject/:id',
    name: 'mobile-reject',
    component: () => import('@/views/MobileApprovalView.vue'),
    meta: { title: '移动审批' },
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFoundView.vue'),
    meta: { title: '未找到' },
  },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior() {
    return { top: 0 }
  },
})

export default router