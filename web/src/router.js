import { createRouter, createWebHistory } from 'vue-router'
import AdminApp from './App.vue'

export const pageRoutes = [
  { path: '/overview', name: 'overview' },
  { path: '/groups', name: 'groups' },
  { path: '/upstreams', name: 'upstreams' },
  { path: '/monitors', name: 'monitors' },
  { path: '/logs', name: 'logs' },
  { path: '/routing', name: 'routing' },
  { path: '/settings', name: 'settings' },
]

export const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    { path: '/', redirect: { name: 'overview' } },
    ...pageRoutes.map(route => ({ ...route, component: AdminApp })),
    { path: '/:pathMatch(.*)*', redirect: { name: 'overview' } },
  ],
  scrollBehavior: () => ({ top: 0 }),
})
