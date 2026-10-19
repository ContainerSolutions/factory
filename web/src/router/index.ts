import {
  createRouter,
  createWebHistory,
  type RouteRecordRaw,
  type RouterHistory,
} from 'vue-router';
import BoardView from '@/views/BoardView.vue';

declare module 'vue-router' {
  interface RouteMeta {
    /** The page's title, before ` · CCF Factory`. */
    title?: string;
    /** Shown without a session (the sign-in page), outside the app's shell. */
    public?: boolean;
  }
}

export const routes: RouteRecordRaw[] = [
  { path: '/', name: 'board', component: BoardView, meta: { title: 'Board' } },
  {
    path: '/workitems/:id',
    name: 'workitem',
    component: () => import('@/views/WorkItemView.vue'),
    props: true,
    meta: { title: 'Work item' },
  },
  {
    path: '/runs/:id',
    name: 'run',
    component: () => import('@/views/RunView.vue'),
    props: true,
    meta: { title: 'Run' },
  },
  {
    path: '/pools',
    name: 'pools',
    component: () => import('@/views/PoolsView.vue'),
    meta: { title: 'Queues and pools' },
  },
  {
    path: '/catalog',
    name: 'catalog',
    component: () => import('@/views/CatalogView.vue'),
    meta: { title: 'Catalog' },
  },
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/LoginView.vue'),
    meta: { title: 'Sign in', public: true },
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFoundView.vue'),
    meta: { title: 'Not found' },
  },
];

export function createAppRouter(history: RouterHistory = createWebHistory()) {
  const router = createRouter({ history, routes });
  router.afterEach((to) => {
    const title = typeof to.meta.title === 'string' ? to.meta.title : '';
    document.title = title ? `${title} · CCF Factory` : 'CCF Factory';
  });
  return router;
}
