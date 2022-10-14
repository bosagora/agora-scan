# Agorascan - Agora consensus layer explorer

The explorer provides a comprehensive and easy to use interface for the Agora consensus layer chain. It makes it easy to view proposed blocks, follow attestations and monitor your staking activity.

## About

The explorer is forked from the Open Source eth2-explorer at https://github.com/gobitfly/eth2-beaconchain-explorer

### Agorascan Testnet Explorer
Testnet: https://testnet.agorascan.io

## Features

- Bootstrap based and mobile first web interface
- Fast and robust blockchain indexing engine, able to handle missed, duplicate & forked blocks
- Index page
  - Auto refresh - Index page data is automatically updated every 15 seconds
  - Basic chain statistics (current epoch, current slot, active validators, pending validators, staked ether)
  - Information on the 20 most recent blocks (epoch, slot, time, proposer, hash, number of attestations, deposits, slahsings and voluntary exits)
- Epochs page
  - Pageable tabular view of all epochs (epoch, time, blocks, attestations, slashings, exits, finalization status, voting statistics)
- Blocks page
  - Pageable tabular view of all blocks (epoch, time, proposer, hash, attestations, slashings, exits)
- Block page
  - Basic block info (epoch, slot, status, time, proposer, root hash, parent hash, state root hash, signature, randao reveal, graffiti, eth1 data)
  - List of attestations included in the block
  - List of deposits included in the block
  - List of LMD GHOST votes
- Validators page
  - Pageable tabular view of all pending, active and ejected validators
- Validator page
  - Basic validator info (index, current balance, current effective balance, status, slashed, active since, exited on)
  - Historic balance evolution chart
  - List of proposed and missed blocks
- Visualizations
  - Live visualization of blocks being added to the blockchain

## ToDo

- Add chain statistic charts
- Improve design, move away from stock bootstrap 4
- Use a proper open source charting library
- Come up with a smarter exporter logic (the current logic is stupid as it simply dumps the contents of the RPC calls into the database without doing any pre-aggregation or cleanups)

## Getting started

We currently do not provide any pre-built binaries of the explorer.
Docker images are available from `Dockerhub.com` by pulling `bosagora/agora-scan:agora`

- Download the latest version of the Agora-cl client and start it with the `--archive` flag set
- Wait till the client finishes the initial sync
- Setup a PostgreSQL DB and import the `tables.sql` file from the root of this repository
- Install go version 1.13 or higher
- Clone the repository and run `make all` to build the indexer and front-end binaries
- Copy the config-example.yml file and adapt it to your environment
- Start the explorer binary and pass the path to the config file as argument
- To build bootstrap run `npm run --prefix ./bootstrap dist-css` in project folder.

## Developing locally with docker
- Clone the repository
- Run `docker-compose up` to start instances of the following containers `agora-el`, `agora-cl`, `postgres` and `golang`.
- Open a new terminal in project directory and run `docker run -it --rm --net=host -v $(pwd):/src  postgres psql -f /src/tables.sql -d db -h 0.0.0.0 -U postgres` to create new tables in the database
- Wait for the client to finish initial sync, you can check this by looking at logs of `agora-cl` instance.
- Copy the `config-example.yml` file and adapt it to your environment.\
 In your `.yml` file specify `eth1Endpoint` as `geth.ipc`.
 For database information check `postgres` section in `docker-compose.yml` file.
- Connect to `golang` instance by running `docker exec -ti golang bash` and run `make all`
- Start the explorer binary and pass the path to the config file as argument

      ./bin/explorer --config your_config.yml

## Development

Install golint. (see https://github.com/golang/lint)

## Commercial usage

The explorer uses Highsoft charts which are not free for commercial and governmental use. If you plan to use the explorer for commercial purposes you currently need to purchase an appropriate HighSoft license.
We are planning to switch out the Highsoft chart library with a less restrictive charting library (suggestions are welcome).
