import { createBrowserRouter } from 'react-router-dom';
import { RequireAuth, RequireAdmin } from './components/auth';
import { Layout } from './pages/Layout';
import { Dashboard } from './pages/Dashboard';
import { Upstreams } from './pages/Upstreams';
import { Events } from './pages/Events';
import { Users } from './pages/Users';
import { Config } from './pages/Config';

export const router = createBrowserRouter([
  {
    element: (
      <RequireAuth>
        <Layout />
      </RequireAuth>
    ),
    children: [
      { path: '/', element: <Dashboard /> },
      { path: '/upstreams', element: <Upstreams /> },
      { path: '/events', element: <Events /> },
      {
        path: '/users',
        element: (
          <RequireAdmin>
            <Users />
          </RequireAdmin>
        ),
      },
      {
        path: '/config',
        element: (
          <RequireAdmin>
            <Config />
          </RequireAdmin>
        ),
      },
    ],
  },
]);
