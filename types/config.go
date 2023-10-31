package types

// Config data
type Topic struct {
	Name        string     `yaml:"name"`
	Description string     `yaml:"description"`
	Emoji       string     `yaml:"emoji"`
	Questions   []Question `yaml:"questions"`
	Ping        []string   `yaml:"ping"`
}

type Question struct {
	Question    string `yaml:"question"`
	Placeholder string `yaml:"placeholder"`
}

type ConfigDatabase struct {
	Postgres        string `yaml:"postgres"`
	Redis           string `yaml:"redis"`
	FileStoragePath string `yaml:"file_storage_path"`
	ExposedPath     string `yaml:"exposed_path"`
}

type ConfigChannels struct {
	ThreadChannel string `yaml:"thread_channel"`
	LogChannel    string `yaml:"log_channel"`
}

type Config struct {
	Topics   map[string]Topic `yaml:"topics"`
	Database ConfigDatabase   `yaml:"database"`
	Channels ConfigChannels   `yaml:"channels"`
}

type Secrets struct {
	Token string `yaml:"token"`
}
