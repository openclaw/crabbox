package cli

//go:generate go run ../../scripts/configgen -source config_sprites.go -output config_sprites_generated.go -type SpritesConfig -provider sprites

// SpritesConfig owns mechanical bindings; provider prevalidation and source
// provenance remain in their existing wrappers.
type SpritesConfig struct {
	Token    string `env:"CRABBOX_SPRITES_TOKEN" envAlias:"SPRITES_TOKEN" envAlias2:"SPRITE_TOKEN" envAlias3:"SETUP_SPRITE_TOKEN" sources:"env" reportApplied:"true"`
	APIURL   string `config:"apiUrl" env:"CRABBOX_SPRITES_API_URL" envAlias:"SPRITES_API_URL" flag:"sprites-api-url" sources:"user,repo,env,flag" help:"Sprites API URL" default:"https://api.sprites.dev" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	WorkRoot string `config:"workRoot" env:"CRABBOX_SPRITES_WORK_ROOT" flag:"sprites-work-root" sources:"user,repo,env,flag" help:"Sprites remote work root" default:"/home/sprite/crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
}

func applySpritesFileConfig(cfg *Config, file *fileSpritesConfig, source configInputSource, credentialSource credentialValueSource) error {
	applied, err := cfg.Sprites.applyFile(file)
	recordConfigInput(cfg, "sprites", source, applied.InputAccepted)
	if applied.APIURL {
		cfg.credentialProvenance.spritesAPIURL = credentialSource
	}
	return err
}

func applySpritesEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Sprites.applyEnv()
	recordConfigInput(cfg, "sprites", configInputEnvironment, applied.InputAccepted)
	if applied.Token {
		cfg.credentialProvenance.spritesToken = credentialSourceEnvironment
	}
	if applied.APIURL {
		cfg.credentialProvenance.spritesAPIURL = credentialSourceEnvironment
	}
	return err
}
