import tseslint from 'typescript-eslint'
import reactHooks from 'eslint-plugin-react-hooks'

export default tseslint.config(
  tseslint.configs.recommended,
  reactHooks.configs['recommended-latest'],
  {
    ignores: ['dist/**'],
  },
)
