/** @type {import('tailwindcss').Config} */
// 主题：工程控制台风（暗色优先、单一等宽字体、仪表琥珀强调色、发丝分割线取代阴影）。
// 把全站大量使用的 `slate` 色阶重映射为暖偏近黑中性灰，`brand` 改为仪表琥珀。
// 950=应用底色 / 900=面板 / 800=边框 / 400=次要文字 / 100=主文字。
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        // 仪表琥珀：唯一交互强调色（主按钮/激活态/焦点环）。
        brand: {
          DEFAULT: '#ffb02e',
          fg: '#1a1200',
        },
        // 暖偏近黑中性灰阶（暗色优先）：
        // 950=应用底色，900=面板，800=边框/输入，700=次级按钮/强分隔，
        // 400=次要文字，100=主文字（暖白，非纯白）。
        slate: {
          50: '#f6f3ee',
          100: '#ece7dd',
          200: '#d3ccc0',
          300: '#b0a696',
          400: '#8c8477',
          500: '#6b6459',
          600: '#4a443b',
          700: '#332e27',
          800: '#1c1a16',
          900: '#131210',
          950: '#0a0908',
        },
      },
      fontFamily: {
        // 全局唯一字体：IBM Plex Mono（系统等宽兜底）。
        sans: ['"IBM Plex Mono"', 'ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace'],
        mono: ['"IBM Plex Mono"', 'ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace'],
      },
      borderRadius: {
        // 圆角收到近乎直角：仪表盘感。
        md: '2px',
        lg: '2px',
        xl: '3px',
      },
    },
  },
  plugins: [],
};
